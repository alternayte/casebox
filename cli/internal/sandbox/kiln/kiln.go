// Package kiln is the sandbox provider on Kiln microVMs (docs/specs/sandboxes.md). An environment
// is a Kiln template named after its key; a sandbox is a Kiln sandbox restored from that template
// or from a Kiln snapshot. It talks to Kiln only through Kiln's Go client.
package kiln

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/kiln/client"
)

// Name is the provider's name in sandbox.Sandbox.Provider and CASEBOX_SANDBOX.
const Name = "kiln"

const (
	// userName is the account uid sandbox.User gets in every template.
	userName = "casebox"
	userHome = "/home/casebox"
	// resultsDir is where test commands write their reports.
	resultsDir = "/results"
	// stateDir holds the provider's per-command files: stdin, stdout, stderr and tar streams. Only
	// root can enter it, so the sandbox user cannot read or replace them.
	stateDir = "/var/lib/casebox"

	// Every template gets the resources StartOptions defaults to; Kiln sets them per template.
	vcpus    = 2
	memoryMB = 4096
	diskMB   = 20480

	// execTimeoutMax is the longest command Kiln runs (guestproto.ExecTimeoutMax).
	execTimeoutMax = 3600
	// requestLimit is the largest JSON body Kiln's API decodes (api.decodeJSON).
	requestLimit = 1 << 20
	// housekeeping caps the provider's own commands: making and removing its files.
	housekeeping = 60 * time.Second
)

// Provider runs sandboxes on one Kiln host or gateway.
type Provider struct {
	c    *client.Client
	poll time.Duration // how often Prepare reads a building template
}

// New returns a provider for the Kiln API at url, authenticated by apiKey.
func New(url, apiKey string) *Provider {
	return &Provider{c: client.New(url, apiKey), poll: 2 * time.Second}
}

// FromEnv returns a provider for KILN_URL and KILN_API_KEY.
func FromEnv() (*Provider, error) {
	url, key := os.Getenv("KILN_URL"), os.Getenv("KILN_API_KEY")
	if url == "" || key == "" {
		return nil, errors.New("the kiln sandbox provider needs KILN_URL and KILN_API_KEY")
	}
	return New(url, key), nil
}

var _ sandbox.Provider = (*Provider)(nil)

// TemplateName is the Kiln template an environment key builds.
func TemplateName(key string) string {
	if len(key) > 16 {
		key = key[:16]
	}
	return "casebox-" + key
}

// Prepare builds the environment as a Kiln template once per key and reuses a ready one after.
func (p *Provider) Prepare(ctx context.Context, spec sandbox.EnvSpec) (sandbox.ImageRef, error) {
	if len(spec.Services) > 0 {
		return sandbox.ImageRef{}, errors.New("kiln: services are not supported: Kiln's template API takes no services")
	}
	key := spec.Key()
	name := TemplateName(key)
	req, err := templateRequest(name, spec)
	if err != nil {
		return sandbox.ImageRef{}, err
	}
	ref := sandbox.ImageRef{ID: name, Key: key, Workdir: spec.Dir()}

	// A template that failed before this call is built again: Kiln accepts a new build for a failed
	// name. A build this call started or waited on that fails is the answer.
	waited := false
	for {
		t, err := p.c.Template(ctx, name)
		switch {
		case client.NotFound(err):
			if err := p.build(ctx, req); err != nil {
				return sandbox.ImageRef{}, err
			}
			waited = true
			continue
		case err != nil:
			return sandbox.ImageRef{}, fmt.Errorf("kiln: read template %s: %w", name, err)
		}
		if t.Image != spec.Image {
			return sandbox.ImageRef{}, fmt.Errorf("kiln: template %s is built from %s, not %s", name, t.Image, spec.Image)
		}
		switch t.State {
		case "ready":
			return ref, nil
		case "failed":
			if waited {
				return sandbox.ImageRef{}, fmt.Errorf("kiln: template %s failed to build: %s", name, t.Error)
			}
			if err := p.build(ctx, req); err != nil {
				return sandbox.ImageRef{}, err
			}
			waited = true
			continue
		case "building":
			waited = true
		default:
			return sandbox.ImageRef{}, fmt.Errorf("kiln: template %s is in unknown state %q", name, t.State)
		}
		select {
		case <-ctx.Done():
			return sandbox.ImageRef{}, fmt.Errorf("kiln: wait for template %s: %w", name, ctx.Err())
		case <-time.After(p.poll):
		}
	}
}

// build starts one template build. A conflict means another caller is building the same name, so
// the caller polls it like its own.
func (p *Provider) build(ctx context.Context, req client.TemplateRequest) error {
	if err := p.c.CreateTemplate(ctx, req); err != nil && !client.Conflict(err) {
		return fmt.Errorf("kiln: build template %s: %w", req.Name, err)
	}
	return nil
}

// templateRequest is the Kiln template for one environment. Setup creates the sandbox user, the
// working directory and the results directory, writes the context files, runs the install steps as
// root in the working directory, and gives the sandbox user what they wrote there.
func templateRequest(name string, spec sandbox.EnvSpec) (client.TemplateRequest, error) {
	dir := spec.Dir()
	if !path.IsAbs(dir) {
		return client.TemplateRequest{}, fmt.Errorf("kiln: the working directory %q is not absolute", dir)
	}
	uid := strconv.Itoa(sandbox.User)
	setup := []string{
		"command -v su >/dev/null || { echo 'the image has no su' >&2; exit 1; }\n" +
			"if ! id -u " + userName + " >/dev/null 2>&1; then\n" +
			"  if command -v useradd >/dev/null 2>&1; then\n" +
			"    groupadd -g " + uid + " " + userName + " && useradd -u " + uid + " -g " + uid + " -m -d " + userHome + " -s /bin/sh " + userName + "\n" +
			"  else\n" +
			"    addgroup -g " + uid + " " + userName + " && adduser -D -u " + uid + " -G " + userName + " -h " + userHome + " -s /bin/sh " + userName + "\n" +
			"  fi\n" +
			"fi\n" +
			"[ \"$(id -u " + userName + ")\" = " + uid + " ] || { echo 'the user " + userName + " does not have uid " + uid + "' >&2; exit 1; }",
		"mkdir -p " + quote(dir) + " " + resultsDir + " " + stateDir + " && chmod 0700 " + stateDir,
	}
	paths := make([]string, 0, len(spec.Context))
	for p := range spec.Context {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		clean := path.Clean(strings.ReplaceAll(rel, "\\", "/"))
		if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return client.TemplateRequest{}, fmt.Errorf("kiln: the context file %q is outside the working directory", rel)
		}
		target := path.Join(dir, clean)
		setup = append(setup, "mkdir -p "+quote(path.Dir(target))+" && printf '%s' "+
			quote(base64.StdEncoding.EncodeToString(spec.Context[rel]))+" | base64 -d > "+quote(target))
	}
	for _, step := range spec.Install {
		setup = append(setup, "cd "+quote(dir)+" || exit 1\n"+step)
	}
	setup = append(setup, "chown -R "+uid+":"+uid+" "+quote(dir)+" "+resultsDir)

	req := client.TemplateRequest{
		Name:        name,
		Image:       spec.Image,
		VCPUs:       vcpus,
		MemoryMB:    memoryMB,
		DiskMB:      diskMB,
		Setup:       setup,
		EgressAllow: []string{}, // no egress: Kiln requires the field and reads an empty list as none
	}
	body, err := json.Marshal(req)
	if err != nil {
		return client.TemplateRequest{}, err
	}
	if len(body) > requestLimit {
		return client.TemplateRequest{}, fmt.Errorf("kiln: the template request is %d bytes with its context files, over Kiln's %d byte limit", len(body), requestLimit)
	}
	return req, nil
}

// Start restores a sandbox from a template or a snapshot.
func (p *Provider) Start(ctx context.Context, from sandbox.Ref, opts sandbox.StartOptions) (sandbox.Sandbox, error) {
	opts = opts.Defaults()
	if len(from.RefServices()) > 0 {
		return sandbox.Sandbox{}, errors.New("kiln: services are not supported: Kiln's template API takes no services")
	}
	if len(opts.Egress) > 0 {
		return sandbox.Sandbox{}, fmt.Errorf("kiln: an egress allow-list per sandbox: %w: Kiln sets egress_allow per template, and casebox templates allow none", sandbox.ErrUnsupported)
	}
	if opts.Network != sandbox.NetworkNone {
		return sandbox.Sandbox{}, fmt.Errorf("kiln: network %q is not supported: Kiln sets egress per template and has no unrestricted egress", opts.Network)
	}
	if opts.CPUs != vcpus || opts.MemoryMB != memoryMB {
		return sandbox.Sandbox{}, fmt.Errorf("kiln: %g CPUs and %d MB are not supported: Kiln sets resources per template, and casebox templates have %d CPUs and %d MB", opts.CPUs, opts.MemoryMB, vcpus, memoryMB)
	}
	meta := map[string]string{}
	if len(opts.Env) > 0 {
		// Kiln marks a sandbox with create-time env as secret-bearing, and a snapshot restore does
		// not take env at all, so the start env travels with the Sandbox and goes on every command.
		env, err := json.Marshal(opts.Env)
		if err != nil {
			return sandbox.Sandbox{}, err
		}
		meta["env"] = string(env)
	}
	workdir := from.RefWorkdir()
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}

	var id string
	switch ref := from.(type) {
	case sandbox.ImageRef:
		ttl := seconds(opts.Lifetime)
		sb, err := p.c.CreateSandbox(ctx, client.SandboxRequest{
			Template:    ref.ID,
			Lifecycle:   "ephemeral",
			IdleSeconds: ttl,
			TTLSeconds:  &ttl,
			Metadata:    map[string]any{"casebox": true},
		})
		if err != nil {
			return sandbox.Sandbox{}, fmt.Errorf("kiln: create a sandbox from template %s: %w", ref.ID, err)
		}
		id = sb.ID
	case sandbox.SnapshotRef:
		// A restored copy keeps the lifecycle and lifetime of the sandbox the snapshot came from:
		// Kiln's restore takes no ttl.
		copies, err := p.c.RestoreSnapshot(ctx, ref.ID, 1, false)
		if err != nil {
			return sandbox.Sandbox{}, fmt.Errorf("kiln: restore snapshot %s: %w", ref.ID, err)
		}
		if len(copies) != 1 {
			return sandbox.Sandbox{}, fmt.Errorf("kiln: restore snapshot %s returned %d sandboxes, want 1", ref.ID, len(copies))
		}
		id = copies[0].ID
	default:
		return sandbox.Sandbox{}, fmt.Errorf("kiln: cannot start from a %T", from)
	}
	return sandbox.Sandbox{ID: id, Provider: Name, Workdir: workdir, Meta: meta}, nil
}

// Snapshot saves a running sandbox as a listed Kiln snapshot. The sandbox keeps running.
func (p *Provider) Snapshot(ctx context.Context, sb sandbox.Sandbox) (sandbox.SnapshotRef, error) {
	if err := mine(sb); err != nil {
		return sandbox.SnapshotRef{}, err
	}
	snap, err := p.c.Snapshot(ctx, sb.ID, false)
	if err != nil {
		return sandbox.SnapshotRef{}, p.fail(ctx, sb.ID, "snapshot", err)
	}
	return sandbox.SnapshotRef{ID: snap.SnapshotID, Workdir: sb.Workdir}, nil
}

// Exec runs one argv as the sandbox user, or as root for the runner. Kiln runs commands as root
// with no stdin and returns at most 1 MiB of output as text, so the provider writes stdin to a
// file, runs the argv through su with its stdin, stdout and stderr on files, and reads the output
// back from those files.
func (p *Provider) Exec(ctx context.Context, sb sandbox.Sandbox, cmd sandbox.Command) (sandbox.ExecResult, error) {
	if err := mine(sb); err != nil {
		return sandbox.ExecResult{}, err
	}
	if len(cmd.Args) == 0 {
		return sandbox.ExecResult{}, errors.New("kiln: exec needs an argv")
	}
	timeout := seconds(cmd.TimeoutOrDefault())
	if timeout > execTimeoutMax {
		return sandbox.ExecResult{}, fmt.Errorf("kiln: a timeout of %s is over Kiln's %d second limit", cmd.TimeoutOrDefault(), execTimeoutMax)
	}
	env, err := commandEnv(sb, cmd)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	files, err := scratch("exec")
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	stdin := "/dev/null"
	if cmd.Stdin != nil {
		stdin = files + "/stdin"
		if err := p.c.WriteFile(ctx, sb.ID, stdin, cmd.Stdin); err != nil {
			return sandbox.ExecResult{}, p.fail(ctx, sb.ID, "write stdin", err)
		}
	}
	dir := cmd.Dir
	if dir == "" {
		dir = sb.Workdir
	}
	start := time.Now()
	res, err := p.c.Exec(ctx, sb.ID, client.ExecRequest{
		Cmd:            []string{"/bin/sh", "-c", execScript(files, stdin, dir, cmd.Args, cmd.Root)},
		Env:            env,
		TimeoutSeconds: timeout,
	})
	took := time.Since(start)
	if err != nil {
		return sandbox.ExecResult{}, p.fail(ctx, sb.ID, "exec", err)
	}
	out := sandbox.ExecResult{ExitCode: res.ExitCode, Duration: took, TimedOut: res.TimedOut}
	var cut bool
	if out.Stdout, cut, err = p.readCapped(ctx, sb.ID, files+"/stdout"); err != nil {
		return sandbox.ExecResult{}, p.unstarted(ctx, sb.ID, cmd.Args, res, err)
	}
	out.Truncated = cut
	if out.Stderr, cut, err = p.readCapped(ctx, sb.ID, files+"/stderr"); err != nil {
		return sandbox.ExecResult{}, p.unstarted(ctx, sb.ID, cmd.Args, res, err)
	}
	out.Truncated = out.Truncated || cut
	if err := p.remove(ctx, sb.ID, files); err != nil {
		return sandbox.ExecResult{}, err
	}
	return out, nil
}

// unstarted explains a command whose output files are missing: the wrapper failed before the
// redirects, and Kiln's own output says why.
func (p *Provider) unstarted(ctx context.Context, id string, args []string, res client.ExecResult, err error) error {
	if client.NotFound(err) {
		if gone, gerr := p.gone(ctx, id); gerr == nil && !gone {
			return fmt.Errorf("kiln: %v did not start: exit %d: %s", args, res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
		}
	}
	return p.fail(ctx, id, "read output", err)
}

// execScript is the root shell that runs one argv with its stdin, stdout and stderr on files.
func execScript(files, stdin, dir string, args []string, root bool) string {
	run := "cd " + quote(dir) + " && exec " + quoteAll(args)
	if !root {
		// -m keeps the environment Kiln set, so the user sees the command's env. su runs as root and
		// needs no password on busybox or util-linux.
		run = "exec su -m -s /bin/sh -c " + quote(run) + " " + userName
	}
	return "mkdir -p -m 0700 " + quote(files) + " || exit 125\n" +
		"{ " + run + "; } <" + quote(stdin) + " >" + quote(files+"/stdout") + " 2>" + quote(files+"/stderr")
}

// commandEnv is the environment of one command: HOME for its user, the start env, then its own.
func commandEnv(sb sandbox.Sandbox, cmd sandbox.Command) (map[string]string, error) {
	env := map[string]string{}
	if !cmd.Root {
		env["HOME"] = userHome
		env["USER"] = userName
		env["LOGNAME"] = userName
	}
	if raw := sb.Meta["env"]; raw != "" {
		var start map[string]string
		if err := json.Unmarshal([]byte(raw), &start); err != nil {
			return nil, fmt.Errorf("kiln: sandbox %s has an unreadable start env: %w", sb.ID, err)
		}
		for k, v := range start {
			env[k] = v
		}
	}
	for k, v := range cmd.Env {
		env[k] = v
	}
	return env, nil
}

// CopyOut returns a tar stream of a file or a directory. tar writes it to a root-only file, which
// Kiln's file API then streams; closing the stream removes the file.
func (p *Provider) CopyOut(ctx context.Context, sb sandbox.Sandbox, target string) (io.ReadCloser, error) {
	if err := mine(sb); err != nil {
		return nil, err
	}
	if !path.IsAbs(target) {
		target = path.Join(sb.Workdir, target)
	}
	target = path.Clean(target)
	parent, base := path.Dir(target), path.Base(target)
	if target == "/" {
		parent, base = "/", "."
	}
	files, err := scratch("copy")
	if err != nil {
		return nil, err
	}
	archive := files + "/out.tar"
	res, err := p.c.Exec(ctx, sb.ID, client.ExecRequest{
		Cmd: []string{"/bin/sh", "-c", "mkdir -p -m 0700 " + quote(files) + " && tar -c -f " + quote(archive) +
			" -C " + quote(parent) + " " + quote(base)},
		TimeoutSeconds: execTimeoutMax,
	})
	if err != nil {
		return nil, p.fail(ctx, sb.ID, "tar "+target, err)
	}
	if res.ExitCode != 0 {
		_ = p.remove(ctx, sb.ID, files)
		return nil, fmt.Errorf("kiln: tar %s: exit %d: %s", target, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	body, err := p.c.ReadFile(ctx, sb.ID, archive)
	if err != nil {
		_ = p.remove(ctx, sb.ID, files)
		return nil, p.fail(ctx, sb.ID, "read "+archive, err)
	}
	return &tarStream{ReadCloser: body, done: func() error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), housekeeping)
		defer cancel()
		return p.remove(ctx, sb.ID, files)
	}}, nil
}

// tarStream removes its file inside the sandbox when it is closed.
type tarStream struct {
	io.ReadCloser
	done func() error
}

func (t *tarStream) Close() error {
	return errors.Join(t.ReadCloser.Close(), t.done())
}

// Destroy removes the sandbox. A sandbox Kiln no longer knows is already destroyed.
func (p *Provider) Destroy(ctx context.Context, sb sandbox.Sandbox) error {
	if err := mine(sb); err != nil {
		return err
	}
	if err := p.c.DeleteSandbox(ctx, sb.ID); err != nil && !client.NotFound(err) {
		return fmt.Errorf("kiln: destroy sandbox %s: %w", sb.ID, err)
	}
	return nil
}

// readCapped reads one output file, keeping at most sandbox.OutputCap bytes.
func (p *Provider) readCapped(ctx context.Context, id, file string) ([]byte, bool, error) {
	r, err := p.c.ReadFile(ctx, id, file)
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, sandbox.OutputCap+1))
	if err != nil {
		return nil, false, fmt.Errorf("kiln: read %s: %w", file, err)
	}
	if len(data) > sandbox.OutputCap {
		return data[:sandbox.OutputCap], true, nil
	}
	return data, false, nil
}

// remove deletes the provider's files of one command.
func (p *Provider) remove(ctx context.Context, id, files string) error {
	res, err := p.c.Exec(ctx, id, client.ExecRequest{
		Cmd:            []string{"rm", "-rf", files},
		TimeoutSeconds: int(housekeeping / time.Second),
	})
	if err != nil {
		return p.fail(ctx, id, "remove "+files, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("kiln: remove %s: exit %d: %s", files, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// fail wraps a Kiln error on one sandbox. Kiln answers 404 for an unknown sandbox and 409 for a
// destroyed one, and 404 for a missing file too, so both are checked against the sandbox row.
func (p *Provider) fail(ctx context.Context, id, what string, err error) error {
	if client.NotFound(err) || client.Conflict(err) {
		if gone, gerr := p.gone(ctx, id); gerr == nil && gone {
			return fmt.Errorf("kiln: %s on sandbox %s: %w", what, id, sandbox.ErrNotFound)
		}
	}
	return fmt.Errorf("kiln: %s on sandbox %s: %w", what, id, err)
}

// gone reports whether Kiln has no live sandbox under id.
func (p *Provider) gone(ctx context.Context, id string) (bool, error) {
	sb, err := p.c.Sandbox(ctx, id)
	if client.NotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return sb.DestroyedAt != nil || sb.State == "destroyed" || sb.State == "failed", nil
}

func mine(sb sandbox.Sandbox) error {
	if sb.Provider != Name {
		return fmt.Errorf("kiln: sandbox %s belongs to provider %q", sb.ID, sb.Provider)
	}
	return nil
}

// scratch is a new directory name for one command's files.
func scratch(kind string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return stateDir + "/" + kind + "-" + hex.EncodeToString(b), nil
}

// seconds rounds a duration up to whole seconds, at least one.
func seconds(d time.Duration) int {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		return 1
	}
	return s
}

// quote makes s one word for sh.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteAll(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = quote(a)
	}
	return strings.Join(out, " ")
}
