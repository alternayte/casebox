// Package docker is the sandbox provider for local trials (docs/specs/sandboxes.md). It drives the
// docker CLI with os/exec, so the CLI keeps no SDK and no CGO. Container isolation is weaker than a
// microVM; Kiln is the provider for untrusted code (SDD section 11).
//
// Every container and network a sandbox owns carries the label casebox.sandbox=<id>, which is how
// Destroy finds them, and casebox.expires=<unix seconds>. The sandbox container runs `sleep
// <lifetime>` as its command under docker-init and is started with --rm, so it stops and removes
// itself when its lifetime ends; the services and the network it leaves behind are removed by the
// next Start.
package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox"
)

// Name is the provider's name in CASEBOX_SANDBOX and in Sandbox.Provider.
const Name = "docker"

// ResultsDir is where test commands write their result files; the sandbox user owns it.
const ResultsDir = "/results"

// PidsLimit caps the processes and threads in one sandbox.
const PidsLimit = 4096

const (
	labelSandbox = "casebox.sandbox"
	labelExpires = "casebox.expires"
	labelKey     = "casebox.key"
	metaNetwork  = "network"
	metaServices = "services"
	metaEnv      = "env:" // start environment, kept out of the container config so snapshots never carry it
	execMarker   = "CASEBOX_EXEC_ID"
)

var serviceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// Provider runs sandboxes as Docker containers.
type Provider struct {
	Binary string // the docker CLI; default "docker"
}

// New returns a provider that runs the docker CLI found on PATH.
func New() *Provider {
	return &Provider{Binary: "docker"}
}

var _ sandbox.Provider = (*Provider)(nil)

// Prepare builds spec into the image casebox-env:<key>, or returns that image when it exists. It
// also pulls the service images, so Start does not wait for them.
func (p *Provider) Prepare(ctx context.Context, spec sandbox.EnvSpec) (sandbox.ImageRef, error) {
	if spec.Image == "" {
		return sandbox.ImageRef{}, errors.New("the environment has no image")
	}
	if !path.IsAbs(spec.Dir()) {
		return sandbox.ImageRef{}, fmt.Errorf("the working directory %q is not absolute", spec.Dir())
	}
	if err := checkServices(spec.Services); err != nil {
		return sandbox.ImageRef{}, err
	}
	key := spec.Key()
	ref := sandbox.ImageRef{
		ID:       "casebox-env:" + key,
		Key:      key,
		Services: append([]sandbox.Service(nil), spec.Services...),
		Workdir:  spec.Dir(),
	}
	for _, svc := range spec.Services {
		if err := p.ensureImage(ctx, svc.Image); err != nil {
			return sandbox.ImageRef{}, fmt.Errorf("service %s: %w", svc.Name, err)
		}
	}
	found, err := p.imageExists(ctx, ref.ID)
	if err != nil || found {
		return ref, err
	}
	if err := p.build(ctx, spec, ref.ID, key); err != nil {
		return sandbox.ImageRef{}, err
	}
	return ref, nil
}

func (p *Provider) build(ctx context.Context, spec sandbox.EnvSpec, tag, key string) error {
	dir, err := os.MkdirTemp("", "casebox-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	dockerfile, err := Dockerfile(spec)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return err
	}
	for name, body := range spec.Context {
		target := filepath.Join(dir, "files", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, p.bin(), "build", "--tag", tag, "--label", labelKey+"="+key, "--file", filepath.Join(dir, "Dockerfile"), dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build %s: %v: %s", tag, err, lastLines(out.String(), 40))
	}
	return nil
}

// Dockerfile is the build Prepare runs for spec: the base image, user 10001, the working directory
// and /results, the context files, the install steps as root, then the sandbox user takes
// ownership of the working directory and /results.
func Dockerfile(spec sandbox.EnvSpec) (string, error) {
	if strings.ContainsAny(spec.Image, "\n\r") {
		return "", fmt.Errorf("the image %q is not one line", spec.Image)
	}
	for name := range spec.Context {
		if err := checkContextPath(name); err != nil {
			return "", err
		}
	}
	dir := spec.Dir()
	uid := strconv.Itoa(sandbox.User)
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\n") }
	run := func(script string) {
		argv, _ := json.Marshal([]string{"/bin/sh", "-c", script})
		line("RUN " + string(argv))
	}
	line("FROM " + spec.Image)
	line("USER root")
	run("grep -q '^[^:]*:[^:]*:" + uid + ":' /etc/passwd || echo 'casebox:x:" + uid + ":" + uid + ":casebox:/home/casebox:/bin/sh' >> /etc/passwd; " +
		"grep -q '^[^:]*:[^:]*:" + uid + ":' /etc/group || echo 'casebox:x:" + uid + ":' >> /etc/group; " +
		"mkdir -p /home/casebox && chown " + uid + ":" + uid + " /home/casebox")
	run("mkdir -p " + shellQuote(dir) + " " + ResultsDir)
	workdir, _ := json.Marshal(dir)
	line("WORKDIR " + string(workdir))
	if len(spec.Context) > 0 {
		copyArgs, _ := json.Marshal([]string{"files/", strings.TrimSuffix(dir, "/") + "/"})
		line("COPY " + string(copyArgs))
	}
	for _, step := range spec.Install {
		run(step)
	}
	run("chown -R " + uid + ":" + uid + " " + shellQuote(dir) + " " + ResultsDir)
	line("USER " + uid)
	return b.String(), nil
}

// Start runs a sandbox from an image or a snapshot: its services first, on the sandbox's own
// network, then the sandbox container, capped and as the sandbox user.
func (p *Provider) Start(ctx context.Context, from sandbox.Ref, opts sandbox.StartOptions) (sandbox.Sandbox, error) {
	if from == nil || from.RefID() == "" {
		return sandbox.Sandbox{}, errors.New("nothing to start the sandbox from")
	}
	if err := checkServices(from.RefServices()); err != nil {
		return sandbox.Sandbox{}, err
	}
	opts = opts.Defaults()
	if opts.Network != sandbox.NetworkNone && opts.Network != sandbox.NetworkOpen {
		return sandbox.Sandbox{}, fmt.Errorf("unknown network %q", opts.Network)
	}
	if err := p.reap(ctx); err != nil {
		return sandbox.Sandbox{}, err
	}
	workdir := from.RefWorkdir()
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	id := "casebox-" + randomHex(8)
	sb := sandbox.Sandbox{ID: id, Provider: Name, Workdir: workdir, Meta: map[string]string{metaNetwork: id}}
	for k, v := range opts.Env {
		sb.Meta[metaEnv+k] = v
	}
	if svcs := from.RefServices(); len(svcs) > 0 {
		raw, err := json.Marshal(svcs)
		if err != nil {
			return sandbox.Sandbox{}, err
		}
		sb.Meta[metaServices] = string(raw)
	}
	seconds := int64(math.Ceil(opts.Lifetime.Seconds()))
	labels := []string{"--label", labelSandbox + "=" + id, "--label", labelExpires + "=" + strconv.FormatInt(time.Now().Unix()+seconds, 10)}

	fail := func(err error) (sandbox.Sandbox, error) {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if derr := p.Destroy(cleanup, sb); derr != nil {
			return sandbox.Sandbox{}, fmt.Errorf("%w (and removing what it started: %v)", err, derr)
		}
		return sandbox.Sandbox{}, err
	}

	netArgs := append([]string{"network", "create"}, labels...)
	if opts.Network == sandbox.NetworkNone {
		netArgs = append(netArgs, "--internal")
	}
	if _, err := p.docker(ctx, nil, append(netArgs, id)...); err != nil {
		return fail(err)
	}
	for _, svc := range from.RefServices() {
		args := append([]string{"run", "--detach", "--name", id + "-" + svc.Name, "--network", id, "--network-alias", svc.Name}, labels...)
		args = append(args, envArgs(svc.Env)...)
		if _, err := p.docker(ctx, nil, append(args, svc.Image)...); err != nil {
			return fail(fmt.Errorf("service %s: %w", svc.Name, err))
		}
	}
	args := append([]string{
		"run", "--detach", "--rm", "--init",
		"--name", id,
		"--network", id,
		"--user", strconv.Itoa(sandbox.User),
		"--workdir", workdir,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(PidsLimit),
		"--cpus", strconv.FormatFloat(opts.CPUs, 'f', -1, 64),
		"--memory", strconv.Itoa(opts.MemoryMB) + "m",
		"--memory-swap", strconv.Itoa(opts.MemoryMB) + "m",
		"--entrypoint", "sleep",
	}, labels...)
	args = append(args, from.RefID(), strconv.FormatInt(seconds, 10))
	if _, err := p.docker(ctx, nil, args...); err != nil {
		return fail(err)
	}
	return sb, nil
}

// Snapshot commits the sandbox's filesystem to casebox-snap:<id>. The sandbox keeps running.
func (p *Provider) Snapshot(ctx context.Context, sb sandbox.Sandbox) (sandbox.SnapshotRef, error) {
	if err := p.ensureRunning(ctx, sb); err != nil {
		return sandbox.SnapshotRef{}, err
	}
	services, err := services(sb)
	if err != nil {
		return sandbox.SnapshotRef{}, err
	}
	tag := "casebox-snap:" + randomHex(8)
	if _, err := p.docker(ctx, nil, "commit", sb.ID, tag); err != nil {
		if errors.Is(err, errNoSuchObject) {
			return sandbox.SnapshotRef{}, sandbox.ErrNotFound
		}
		return sandbox.SnapshotRef{}, err
	}
	return sandbox.SnapshotRef{ID: tag, Services: services, Workdir: sb.Workdir}, nil
}

// services are the services a sandbox started with, as Start recorded them in its Meta.
func services(sb sandbox.Sandbox) ([]sandbox.Service, error) {
	raw := sb.Meta[metaServices]
	if raw == "" {
		return nil, nil
	}
	var out []sandbox.Service
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("read the services of sandbox %s: %w", sb.ID, err)
	}
	return out, nil
}

// Exec runs one argv in the sandbox. A command past its timeout, or whose context ends, is killed
// inside the container together with every process it started: each exec carries a unique
// CASEBOX_EXEC_ID in its environment, which its children inherit, and the kill finds them by it.
func (p *Provider) Exec(ctx context.Context, sb sandbox.Sandbox, cmd sandbox.Command) (sandbox.ExecResult, error) {
	if len(cmd.Args) == 0 {
		return sandbox.ExecResult{}, errors.New("no command to run")
	}
	if sb.ID == "" {
		return sandbox.ExecResult{}, sandbox.ErrNotFound
	}
	user := strconv.Itoa(sandbox.User)
	if cmd.Root {
		user = "0"
	}
	dir := cmd.Dir
	if dir == "" {
		dir = sb.Workdir
	}
	if dir == "" {
		dir = sandbox.DefaultWorkdir
	}
	marker := randomHex(12)
	env := map[string]string{}
	for k, v := range sb.Meta {
		if name, ok := strings.CutPrefix(k, metaEnv); ok {
			env[name] = v
		}
	}
	for k, v := range cmd.Env {
		env[k] = v
	}
	env[execMarker] = marker
	args := []string{"exec", "--user", user, "--workdir", dir}
	if cmd.Stdin != nil {
		args = append(args, "--interactive")
	}
	args = append(args, envArgs(env)...)
	args = append(append(args, sb.ID), cmd.Args...)

	c := exec.Command(p.bin(), args...)
	var stdout, stderr capped
	c.Stdin, c.Stdout, c.Stderr = cmd.Stdin, &stdout, &stderr
	c.WaitDelay = 10 * time.Second
	start := time.Now()
	if err := c.Start(); err != nil {
		return sandbox.ExecResult{}, fmt.Errorf("docker exec: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	timer := time.NewTimer(cmd.TimeoutOrDefault())
	defer timer.Stop()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-timer.C:
		timedOut = true
		waitErr = p.stop(sb.ID, user, marker, c, done)
	case <-ctx.Done():
		_ = p.stop(sb.ID, user, marker, c, done)
		return sandbox.ExecResult{}, ctx.Err()
	}
	res := sandbox.ExecResult{
		Stdout:    stdout.buf.Bytes(),
		Stderr:    stderr.buf.Bytes(),
		Truncated: stdout.truncated || stderr.truncated,
		Duration:  time.Since(start),
		TimedOut:  timedOut,
	}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		if res.ExitCode < 0 {
			res.ExitCode = 137 // the docker CLI itself was killed
		}
	default:
		return sandbox.ExecResult{}, fmt.Errorf("docker exec: %w", waitErr)
	}
	if res.ExitCode != 0 && !timedOut && strings.Contains(string(res.Stderr), "Error response from daemon") {
		// The daemon refused the exec, or the command printed the same words: only the container's
		// state tells which.
		if err := p.ensureRunning(ctx, sb); err != nil {
			return sandbox.ExecResult{}, err
		}
	}
	return res, nil
}

// stop kills every process of one exec inside the container, then waits for the docker CLI; if the
// CLI does not return, it is killed too.
func (p *Provider) stop(container, user, marker string, c *exec.Cmd, done <-chan error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	script := `for round in 1 2 3; do for d in /proc/[0-9]*; do ` +
		`grep -q "` + execMarker + `=$0" "$d/environ" 2>/dev/null && kill -9 "${d#/proc/}" 2>/dev/null; ` +
		`done; done; true`
	_, _ = p.docker(ctx, nil, "exec", "--user", user, container, "sh", "-c", script, marker)
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		_ = c.Process.Kill()
		return <-done
	}
}

// CopyOut streams a tar of a file or directory in the sandbox.
func (p *Provider) CopyOut(ctx context.Context, sb sandbox.Sandbox, path string) (io.ReadCloser, error) {
	if err := p.ensureRunning(ctx, sb); err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, p.bin(), "cp", sb.ID+":"+path, "-")
	r := &copyStream{cmd: c, path: path}
	c.Stderr = &r.stderr
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	r.out = out
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("docker cp: %w", err)
	}
	return r, nil
}

type copyStream struct {
	cmd    *exec.Cmd
	path   string
	out    io.ReadCloser
	stderr bytes.Buffer
	waited bool
	err    error
}

func (r *copyStream) Read(b []byte) (int, error) {
	n, err := r.out.Read(b)
	if errors.Is(err, io.EOF) {
		if werr := r.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (r *copyStream) Close() error {
	if r.waited {
		return nil
	}
	_ = r.out.Close()
	if r.cmd.ProcessState == nil {
		_ = r.cmd.Process.Kill()
	}
	_ = r.wait()
	return nil
}

func (r *copyStream) wait() error {
	if !r.waited {
		r.waited = true
		if err := r.cmd.Wait(); err != nil {
			r.err = fmt.Errorf("docker cp %s: %v: %s", r.path, err, strings.TrimSpace(r.stderr.String()))
		}
	}
	return r.err
}

// Destroy removes the sandbox, its services and its network. Destroying twice is not an error.
func (p *Provider) Destroy(ctx context.Context, sb sandbox.Sandbox) error {
	if sb.ID == "" {
		return nil
	}
	filter := "label=" + labelSandbox + "=" + sb.ID
	for deadline := time.Now().Add(time.Minute); ; {
		out, err := p.docker(ctx, nil, "ps", "--all", "--quiet", "--filter", filter)
		if err != nil {
			return err
		}
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the containers of sandbox %s are still there: %s", sb.ID, strings.Join(ids, " "))
		}
		// A container that --rm is already removing, or that went away since the listing, is
		// fine; the next listing shows whether anything is left.
		if _, err := p.docker(ctx, nil, append([]string{"rm", "--force", "--volumes"}, ids...)...); err != nil &&
			!errors.Is(err, errNoSuchObject) && !strings.Contains(err.Error(), "already in progress") {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	out, err := p.docker(ctx, nil, "network", "ls", "--quiet", "--filter", filter)
	if err != nil {
		return err
	}
	if nets := strings.Fields(string(out)); len(nets) > 0 {
		if _, err := p.docker(ctx, nil, append([]string{"network", "rm"}, nets...)...); err != nil && !errors.Is(err, errNoSuchObject) {
			return err
		}
	}
	return nil
}

// reap destroys the sandboxes whose lifetime has ended. Their containers stopped and removed
// themselves; their services and networks are still there.
func (p *Provider) reap(ctx context.Context) error {
	expired := map[string]bool{}
	now := time.Now().Unix()
	for _, list := range [][]string{
		{"ps", "--all"},
		{"network", "ls"},
	} {
		args := append(list, "--filter", "label="+labelExpires, "--format", `{{.Label "`+labelSandbox+`"}} {{.Label "`+labelExpires+`"}}`)
		out, err := p.docker(ctx, nil, args...)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			id, at, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || id == "" {
				continue
			}
			if unix, err := strconv.ParseInt(at, 10, 64); err == nil && unix < now {
				expired[id] = true
			}
		}
	}
	for id := range expired {
		if err := p.Destroy(ctx, sandbox.Sandbox{ID: id}); err != nil {
			return fmt.Errorf("remove the expired sandbox %s: %w", id, err)
		}
	}
	return nil
}

// ensureRunning returns sandbox.ErrNotFound unless the sandbox container is running. A container
// that stopped has reached its lifetime and is being removed.
func (p *Provider) ensureRunning(ctx context.Context, sb sandbox.Sandbox) error {
	if sb.ID == "" {
		return sandbox.ErrNotFound
	}
	out, err := p.docker(ctx, nil, "container", "inspect", "--format", "{{.State.Running}}", sb.ID)
	if errors.Is(err, errNoSuchObject) {
		return sandbox.ErrNotFound
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "true" {
		return sandbox.ErrNotFound
	}
	return nil
}

func (p *Provider) imageExists(ctx context.Context, image string) (bool, error) {
	_, err := p.docker(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image)
	if errors.Is(err, errNoSuchObject) {
		return false, nil
	}
	return err == nil, err
}

func (p *Provider) ensureImage(ctx context.Context, image string) error {
	found, err := p.imageExists(ctx, image)
	if err != nil || found {
		return err
	}
	_, err = p.docker(ctx, nil, "pull", "--quiet", image)
	return err
}

// errNoSuchObject marks a docker error about a container, image or network that does not exist.
var errNoSuchObject = errors.New("no such object")

// docker runs the docker CLI and returns its standard output.
func (p *Provider) docker(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.bin(), args...)
	cmd.Stdin = stdin
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if isNoSuch(msg) {
			return nil, fmt.Errorf("docker %s: %w: %s", args[0], errNoSuchObject, msg)
		}
		return nil, fmt.Errorf("docker %s: %v: %s", args[0], err, msg)
	}
	return out.Bytes(), nil
}

func isNoSuch(msg string) bool {
	lower := strings.ToLower(msg)
	for _, s := range []string{"no such container", "no such image", "no such network", "no such object"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

func (p *Provider) bin() string {
	if p.Binary == "" {
		return "docker"
	}
	return p.Binary
}

// capped keeps the first sandbox.OutputCap bytes written to it.
type capped struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *capped) Write(b []byte) (int, error) {
	room := sandbox.OutputCap - c.buf.Len()
	if len(b) > room {
		c.buf.Write(b[:max(room, 0)])
		c.truncated = true
		return len(b), nil
	}
	c.buf.Write(b)
	return len(b), nil
}

func envArgs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, "--env", k+"="+env[k])
	}
	return args
}

func checkServices(services []sandbox.Service) error {
	seen := map[string]bool{}
	for _, svc := range services {
		if !serviceName.MatchString(svc.Name) {
			return fmt.Errorf("the service name %q is not a valid host name", svc.Name)
		}
		if seen[svc.Name] {
			return fmt.Errorf("two services are named %q", svc.Name)
		}
		seen[svc.Name] = true
		if svc.Image == "" {
			return fmt.Errorf("the service %s has no image", svc.Name)
		}
	}
	return nil
}

func checkContextPath(name string) error {
	clean := path.Clean(name)
	if name == "" || path.IsAbs(name) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(name, `\`) {
		return fmt.Errorf("the context file %q is not a relative path inside the repository", name)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Check says whether this machine's Docker daemon can run sandboxes: it must answer, and run
// Linux containers (a Windows daemon in Windows-container mode cannot).
func (p *Provider) Check(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, p.bin(), "info", "--format", "{{.OSType}}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker does not answer: %s", strings.TrimSpace(string(out)))
	}
	if os := strings.TrimSpace(string(out)); os != "linux" {
		return fmt.Errorf("the Docker daemon runs %s containers; Casebox sandboxes need Linux containers", os)
	}
	return nil
}
