// Package daytona is the sandbox provider on Daytona (docs/specs/sandboxes.md, SDD section 12).
// It talks to Daytona's REST APIs with net/http; Daytona's Go SDK pulls in the AWS SDK and
// OpenTelemetry and its source moved to a private repository in June 2026.
//
// Daytona API surface this provider relies on, read 2026-09-29 from
// https://www.daytona.io/docs/openapi.json (platform API),
// https://www.daytona.io/docs/toolbox-openapi.json (toolbox API),
// https://www.daytona.io/docs/en/sandboxes.md and https://www.daytona.io/docs/en/snapshots.md,
// with runner, daemon and API behaviour the specs leave open read from the public source at
// github.com/daytonaio/daytona tag v0.190.0 (apps/runner, apps/daemon, apps/api).
//
// Platform API, under DAYTONA_API_URL, header Authorization: Bearer <API key>:
//
//	GET    /object-storage/push-access      -> {accessKey, secret, sessionToken, storageUrl, organizationId, bucket}
//	POST   /snapshots                       {name, imageName, entrypoint, buildInfo{dockerfileContent, contextHashes}} -> SnapshotDto; 409 when the name exists
//	GET    /snapshots/{idOrName}            -> SnapshotDto {id, name, state, errorReason}; 404
//	POST   /snapshots/{id}/activate         an inactive snapshot becomes pending, then active
//	DELETE /snapshots/{id}
//	POST   /sandbox                         {snapshot, env, labels, networkBlockAll, autoStopInterval, autoDeleteInterval, ttlMinutes, linkedSandbox} -> Sandbox
//	GET    /sandbox/{idOrName}              -> Sandbox {id, state, errorReason, cpu, memory, toolboxProxyUrl}; 404
//	POST   /sandbox/{idOrName}/resize       {cpu, memory}; a started sandbox can only grow
//	POST   /sandbox/{idOrName}/stop
//	POST   /sandbox/{idOrName}/start
//	POST   /sandbox/{idOrName}/snapshot     {name}; a container sandbox may have to be stopped first (400 otherwise)
//	DELETE /sandbox/{idOrName}              deletes linked sandboxes with it
//
// Snapshot states: pending, building, pulling, snapshotting, active, inactive, error,
// build_failed, removing. Sandbox states used: creating, restoring, starting, started,
// stopping, stopped, resizing, snapshotting, pulling_snapshot, pending_build,
// building_snapshot, error, build_failed, destroying, destroyed. Errors are JSON
// {statusCode, message, error}, where message is a string or a list of strings.
//
// Object storage: PUT <storageUrl>/<bucket>/<organizationId>/<md5>/context.tar with AWS
// Signature V4 and the session token; the runner adds each contextHashes entry's tar to the
// build context.
//
// Toolbox API, under <toolboxProxyUrl>/<sandbox id>, same bearer key:
//
//	POST /process/execute      {command, timeout (seconds)} -> {exitCode, result}; 408 ErrorResponse when the timeout killed the process group
//	POST /files/upload-v2?path= raw request body; parent directories are created
//	GET  /files/download?path= file bytes; 404 when missing
//
// The command is piped to the daemon's shell, which runs as the image's user (root here);
// stdout and stderr come back merged, so Exec redirects them to files and downloads those.
package daytona

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox"
)

// Name is the provider's name in CASEBOX_SANDBOX and in sandbox.Sandbox.Provider.
const Name = "daytona"

// DefaultAPIURL is Daytona cloud.
const DefaultAPIURL = "https://app.daytona.io/api"

// userName is the account the build creates for sandbox.User.
const userName = "casebox"

// Meta keys of a started sandbox.
const (
	metaToolbox  = "toolbox"           // the toolbox base URL
	metaNetwork  = "network"           // sandbox.Network of the sandbox
	metaLifetime = "lifetime"          // minutes, for service sandboxes started later
	metaServices = "services"          // JSON []sandbox.Service
	metaLinked   = "service-sandboxes" // JSON map of service name to Daytona sandbox ID
)

// Provider runs sandboxes on Daytona.
type Provider struct {
	c    *client
	poll time.Duration
	// registry returns the base URL of an image registry host.
	registry func(domain string) string
}

var _ sandbox.Provider = (*Provider)(nil)

// New returns a provider for one Daytona API URL and API key.
func New(apiURL, apiKey string) *Provider {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	return &Provider{
		c:        &client{api: strings.TrimRight(apiURL, "/"), key: apiKey, http: &http.Client{}},
		poll:     2 * time.Second,
		registry: func(domain string) string { return "https://" + domain },
	}
}

// FromEnv reads DAYTONA_API_KEY and DAYTONA_API_URL (default Daytona cloud).
func FromEnv() (*Provider, error) {
	key := os.Getenv("DAYTONA_API_KEY")
	if key == "" {
		return nil, errors.New("DAYTONA_API_KEY is not set; the daytona sandbox provider needs an API key")
	}
	return New(os.Getenv("DAYTONA_API_URL"), key), nil
}

// Prepare builds the environment as a Daytona snapshot named casebox-<first 16 of the key>,
// and a snapshot per service image. An active snapshot of the same name is reused.
func (p *Provider) Prepare(ctx context.Context, spec sandbox.EnvSpec) (sandbox.ImageRef, error) {
	if err := validateSpec(spec); err != nil {
		return sandbox.ImageRef{}, err
	}
	for _, svc := range spec.Services {
		if _, err := p.serviceSnapshot(ctx, svc); err != nil {
			return sandbox.ImageRef{}, err
		}
	}
	key := spec.Key()
	name := "casebox-" + key[:16]
	err := p.ensureSnapshot(ctx, name, func(ctx context.Context) (createSnapshot, error) {
		info := &buildInfo{DockerfileContent: dockerfile(spec)}
		if len(spec.Context) > 0 {
			archive, err := contextTar(spec.Context)
			if err != nil {
				return createSnapshot{}, err
			}
			hash, err := p.uploadContext(ctx, archive)
			if err != nil {
				return createSnapshot{}, err
			}
			info.ContextHashes = []string{hash}
		}
		return createSnapshot{BuildInfo: info}, nil
	})
	if err != nil {
		return sandbox.ImageRef{}, err
	}
	return sandbox.ImageRef{ID: name, Key: key, Services: spec.Services, Workdir: spec.Dir()}, nil
}

var serviceName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func validateSpec(spec sandbox.EnvSpec) error {
	if spec.Image == "" || strings.ContainsAny(spec.Image, " \t\r\n") {
		return fmt.Errorf("the environment image %q is not a valid image name", spec.Image)
	}
	dir := spec.Dir()
	if !strings.HasPrefix(dir, "/") || strings.ContainsAny(dir, "\r\n") {
		return fmt.Errorf("the working directory %q is not an absolute path", dir)
	}
	for _, step := range spec.Install {
		if strings.TrimSpace(step) == "" {
			return errors.New("an install step is empty")
		}
	}
	return validateServices(spec.Services)
}

func validateServices(services []sandbox.Service) error {
	seen := map[string]bool{}
	for _, svc := range services {
		if !serviceName.MatchString(svc.Name) {
			return fmt.Errorf("the service name %q is not a valid host name", svc.Name)
		}
		if seen[svc.Name] {
			return fmt.Errorf("the service %q is listed twice", svc.Name)
		}
		seen[svc.Name] = true
		if _, err := parseImage(svc.Image); err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		for k := range svc.Env {
			if !envName.MatchString(k) {
				return fmt.Errorf("service %s: %q is not a valid environment variable name", svc.Name, k)
			}
		}
	}
	return nil
}

// createUser adds sandbox.User as casebox, with busybox or shadow tools, and checks for su,
// which Exec uses to drop from the daemon's root to that user.
var createUser = strings.Join([]string{
	"set -e",
	"if grep -q '^[^:]*:[^:]*:10001:' /etc/passwd; then echo 'casebox: uid 10001 already exists in the base image' >&2; exit 1; fi",
	"if command -v useradd >/dev/null 2>&1; then",
	"  grep -q '^[^:]*:[^:]*:10001:' /etc/group || groupadd -g 10001 casebox",
	"  useradd -u 10001 -g 10001 -m -d /home/casebox -s /bin/sh casebox",
	"else",
	"  grep -q '^[^:]*:[^:]*:10001:' /etc/group || addgroup -g 10001 casebox",
	"  adduser -D -u 10001 -G \"$(awk -F: '$3 == 10001 { print $1; exit }' /etc/group)\" -h /home/casebox -s /bin/sh casebox",
	"fi",
	"command -v su >/dev/null 2>&1 || { echo 'casebox: the base image has no su; the Daytona provider runs commands as uid 10001 through it' >&2; exit 1; }",
}, "\n")

// dockerfile is the build for an EnvSpec, in the order the spec fixes: the base image, user
// 10001, the working directory and /results, the context files, the install steps as root in
// the working directory, then ownership of the working directory and /results for user 10001.
// Its ENTRYPOINT is what Daytona runs beside its daemon: nothing but a sleep.
func dockerfile(spec sandbox.EnvSpec) string {
	dir := spec.Dir()
	var b strings.Builder
	b.WriteString("FROM " + spec.Image + "\n")
	b.WriteString("USER root\n")
	b.WriteString("RUN " + execForm("/bin/sh", "-c", createUser) + "\n")
	b.WriteString("RUN " + execForm("mkdir", "-p", dir, "/results") + "\n")
	if len(spec.Context) > 0 {
		b.WriteString("COPY " + execForm(contextDir+"/", strings.TrimRight(dir, "/")+"/") + "\n")
	}
	b.WriteString("WORKDIR " + dir + "\n")
	for _, step := range spec.Install {
		b.WriteString("RUN " + execForm("/bin/sh", "-c", step) + "\n")
	}
	b.WriteString("RUN " + execForm("chown", "-R", "10001:10001", dir, "/results") + "\n")
	b.WriteString("ENTRYPOINT " + execForm("sleep", "infinity") + "\n")
	return b.String()
}

// execForm is a Dockerfile JSON argument list; it carries any step text, newlines included.
func execForm(args ...string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(args)
	return strings.TrimSpace(b.String())
}

// serviceSnapshot makes sure the snapshot of a service image exists and is active. The
// snapshot's entrypoint is the image's ENTRYPOINT and CMD.
func (p *Provider) serviceSnapshot(ctx context.Context, svc sandbox.Service) (string, error) {
	sum := sha256.Sum256([]byte(svc.Image))
	name := "casebox-svc-" + hex.EncodeToString(sum[:])[:16]
	err := p.ensureSnapshot(ctx, name, func(ctx context.Context) (createSnapshot, error) {
		command, err := p.imageCommand(ctx, svc.Image)
		if err != nil {
			return createSnapshot{}, fmt.Errorf("service %s: %w", svc.Name, err)
		}
		return createSnapshot{ImageName: svc.Image, Entrypoint: command}, nil
	})
	if err != nil {
		return "", fmt.Errorf("service %s: %w", svc.Name, err)
	}
	return name, nil
}

// ensureSnapshot returns once the snapshot called name is active, creating it with build when
// it does not exist. A snapshot found failed from an earlier attempt is deleted and built once
// more; a build that fails in this call is returned as an error with Daytona's reason.
func (p *Provider) ensureSnapshot(ctx context.Context, name string, build func(context.Context) (createSnapshot, error)) error {
	created, rebuilt := false, false
	for {
		var snap snapshotDTO
		err := p.c.call(ctx, http.MethodGet, p.c.platform("/snapshots/"+url.PathEscape(name)), nil, &snap)
		if statusOf(err) == http.StatusNotFound {
			req, err := build(ctx)
			if err != nil {
				return err
			}
			req.Name = name
			err = p.c.call(ctx, http.MethodPost, p.c.platform("/snapshots"), req, &snap)
			if statusOf(err) == http.StatusConflict {
				continue // another worker created it; wait for theirs
			}
			if err != nil {
				return fmt.Errorf("create the Daytona snapshot %s: %w", name, err)
			}
			created = true
		} else if err != nil {
			return fmt.Errorf("read the Daytona snapshot %s: %w", name, err)
		}

		switch snap.State {
		case "active":
			return nil
		case "error", "build_failed":
			if created || rebuilt {
				return fmt.Errorf("the Daytona snapshot %s failed (%s): %s", name, snap.State, reason(snap.ErrorReason))
			}
			if err := p.c.call(ctx, http.MethodDelete, p.c.platform("/snapshots/"+url.PathEscape(snap.ID)), nil, nil); err != nil && statusOf(err) != http.StatusNotFound {
				return fmt.Errorf("delete the failed Daytona snapshot %s: %w", name, err)
			}
			rebuilt = true
		case "inactive":
			err := p.c.call(ctx, http.MethodPost, p.c.platform("/snapshots/"+url.PathEscape(snap.ID)+"/activate"), nil, nil)
			if err != nil && statusOf(err) != http.StatusBadRequest {
				return fmt.Errorf("activate the Daytona snapshot %s: %w", name, err)
			}
		}
		if err := p.sleep(ctx); err != nil {
			return fmt.Errorf("wait for the Daytona snapshot %s (%s): %w", name, snap.State, err)
		}
	}
}

func reason(s string) string {
	if s == "" {
		return "Daytona gave no reason"
	}
	return s
}

func (p *Provider) sleep(ctx context.Context) error {
	t := time.NewTimer(p.poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Start creates a sandbox from an ImageRef's or a SnapshotRef's Daytona snapshot. Daytona
// takes CPU and memory from the snapshot, so a sandbox that starts smaller than asked is
// resized. The lifetime is a wall-clock TTL; auto-stop and auto-delete after the same span of
// inactivity back it up. Network none is Daytona's networkBlockAll.
func (p *Provider) Start(ctx context.Context, from sandbox.Ref, opts sandbox.StartOptions) (sandbox.Sandbox, error) {
	if from == nil || from.RefID() == "" {
		return sandbox.Sandbox{}, errors.New("start needs a prepared environment or a snapshot")
	}
	opts = opts.Defaults()
	if opts.Network != sandbox.NetworkNone && opts.Network != sandbox.NetworkOpen {
		return sandbox.Sandbox{}, fmt.Errorf("the network %q is not none or open", opts.Network)
	}
	if len(opts.Egress) > 0 {
		return sandbox.Sandbox{}, fmt.Errorf("daytona: an egress allow-list of host names: %w: Daytona's network allow list takes IPv4 CIDR blocks only", sandbox.ErrUnsupported)
	}
	if opts.Mirror != nil {
		return sandbox.Sandbox{}, fmt.Errorf("daytona: a registry mirror: %w: Daytona cannot run the egress proxy beside a sandbox, and its network allow list takes IPv4 CIDR blocks only", sandbox.ErrUnsupported)
	}
	services := from.RefServices()
	if err := validateServices(services); err != nil {
		return sandbox.Sandbox{}, err
	}
	for k := range opts.Env {
		if !envName.MatchString(k) {
			return sandbox.Sandbox{}, fmt.Errorf("%q is not a valid environment variable name", k)
		}
	}
	minutes := int(math.Ceil(opts.Lifetime.Minutes()))
	block := opts.Network == sandbox.NetworkNone

	var created sandboxDTO
	err := p.c.call(ctx, http.MethodPost, p.c.platform("/sandbox"), createSandbox{
		Snapshot:           from.RefID(),
		Env:                opts.Env,
		Labels:             map[string]string{"casebox": "sandbox"},
		NetworkBlockAll:    block,
		AutoStopInterval:   intp(minutes),
		AutoDeleteInterval: intp(minutes),
		TTLMinutes:         intp(minutes),
	}, &created)
	if err != nil {
		return sandbox.Sandbox{}, fmt.Errorf("create a Daytona sandbox from %s: %w", from.RefID(), err)
	}
	workdir := from.RefWorkdir()
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	encoded, err := json.Marshal(services)
	if err != nil {
		return sandbox.Sandbox{}, err
	}
	sb := sandbox.Sandbox{ID: created.ID, Provider: Name, Workdir: workdir, Meta: map[string]string{
		metaNetwork:  string(opts.Network),
		metaLifetime: strconv.Itoa(minutes),
		metaServices: string(encoded),
	}}
	ok := false
	defer func() {
		if !ok {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			defer cancel()
			_ = p.Destroy(cleanup, sb)
		}
	}()

	running, err := p.waitState(ctx, created.ID, "started")
	if err != nil {
		return sandbox.Sandbox{}, err
	}
	sb.Meta[metaToolbox] = toolboxBase(running)
	cpu := int(math.Ceil(opts.CPUs))
	memory := int(math.Ceil(float64(opts.MemoryMB) / 1024))
	if running.CPU < float64(cpu) || running.Memory < float64(memory) {
		size := resizeSandbox{CPU: max(cpu, int(running.CPU)), Memory: max(memory, int(running.Memory))}
		if err := p.c.call(ctx, http.MethodPost, p.c.platform("/sandbox/"+url.PathEscape(sb.ID)+"/resize"), size, nil); err != nil {
			return sandbox.Sandbox{}, fmt.Errorf("resize the Daytona sandbox %s to %d CPUs and %d GB: %w", sb.ID, size.CPU, size.Memory, err)
		}
		if _, err := p.waitState(ctx, sb.ID, "started"); err != nil {
			return sandbox.Sandbox{}, err
		}
	}
	if len(services) > 0 {
		if err := p.ensureServices(ctx, sb, services); err != nil {
			return sandbox.Sandbox{}, err
		}
	}
	ok = true
	return sb, nil
}

func toolboxBase(s sandboxDTO) string {
	return strings.TrimRight(s.ToolboxProxyURL, "/") + "/" + url.PathEscape(s.ID)
}

// waitState polls a sandbox until it reaches one of want. A failed or destroyed sandbox ends
// the wait with an error; a sandbox that no longer exists gives sandbox.ErrNotFound.
func (p *Provider) waitState(ctx context.Context, id string, want ...string) (sandboxDTO, error) {
	for {
		s, err := p.getSandbox(ctx, id)
		if err != nil {
			return s, err
		}
		for _, w := range want {
			if s.State == w {
				return s, nil
			}
		}
		switch s.State {
		case "error", "build_failed":
			return s, fmt.Errorf("the Daytona sandbox %s is in state %s: %s", id, s.State, reason(s.ErrorReason))
		case "destroyed", "destroying":
			return s, fmt.Errorf("%w: the Daytona sandbox %s is %s", sandbox.ErrNotFound, id, s.State)
		}
		if err := p.sleep(ctx); err != nil {
			return s, fmt.Errorf("wait for the Daytona sandbox %s to be %s (it is %s): %w", id, strings.Join(want, " or "), s.State, err)
		}
	}
}

func (p *Provider) getSandbox(ctx context.Context, id string) (sandboxDTO, error) {
	var s sandboxDTO
	err := p.c.call(ctx, http.MethodGet, p.c.platform("/sandbox/"+url.PathEscape(id)), nil, &s)
	if statusOf(err) == http.StatusNotFound {
		return s, fmt.Errorf("%w: the Daytona sandbox %s", sandbox.ErrNotFound, id)
	}
	if err != nil {
		return s, fmt.Errorf("read the Daytona sandbox %s: %w", id, err)
	}
	return s, nil
}

// ensureServices starts each service that is not running as a linked Daytona sandbox, which
// shares a link network with the sandbox, and maps the service's name to it in /etc/hosts.
// Daytona requires linked sandboxes to be ephemeral; their TTL is the sandbox's lifetime and
// deleting the sandbox deletes them. sb.Meta records their IDs.
func (p *Provider) ensureServices(ctx context.Context, sb sandbox.Sandbox, services []sandbox.Service) error {
	linked := map[string]string{}
	if raw := sb.Meta[metaLinked]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &linked); err != nil {
			return fmt.Errorf("the sandbox's service record is damaged: %w", err)
		}
	}
	minutes, _ := strconv.Atoi(sb.Meta[metaLifetime])
	sorted := append([]sandbox.Service(nil), services...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, svc := range sorted {
		if id := linked[svc.Name]; id != "" {
			s, err := p.getSandbox(ctx, id)
			if err == nil && s.State == "started" {
				continue
			}
			if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
				return err
			}
		}
		snap, err := p.serviceSnapshot(ctx, svc)
		if err != nil {
			return err
		}
		var child sandboxDTO
		err = p.c.call(ctx, http.MethodPost, p.c.platform("/sandbox"), createSandbox{
			Snapshot:           snap,
			Env:                svc.Env,
			Labels:             map[string]string{"casebox": "service", "casebox-service": svc.Name, "casebox-sandbox": sb.ID},
			NetworkBlockAll:    sb.Meta[metaNetwork] != string(sandbox.NetworkOpen),
			AutoStopInterval:   intp(0),
			AutoDeleteInterval: intp(0),
			TTLMinutes:         intp(minutes),
			LinkedSandbox:      sb.ID,
		}, &child)
		if err != nil {
			return fmt.Errorf("start the service %s: %w", svc.Name, err)
		}
		linked[svc.Name] = child.ID
		encoded, _ := json.Marshal(linked)
		sb.Meta[metaLinked] = string(encoded)
		if _, err := p.waitState(ctx, child.ID, "started"); err != nil {
			return fmt.Errorf("start the service %s: %w", svc.Name, err)
		}
	}
	res, err := p.execute(ctx, sb, hostsScript(sorted, linked), 120)
	if err != nil {
		return fmt.Errorf("map the services to their names: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("map the services to their names: %s", strings.TrimSpace(res.Result))
	}
	return nil
}

// hostsScript resolves each service's Daytona sandbox ID on the link network and writes
// "<ip> <service name>" lines to /etc/hosts, replacing the lines it wrote before.
func hostsScript(services []sandbox.Service, linked map[string]string) string {
	lines := []string{
		"set -e",
		"grep -v ' # casebox-service$' /etc/hosts > /tmp/casebox-hosts </dev/null || true",
	}
	for _, svc := range services {
		id := quote(linked[svc.Name])
		lines = append(lines,
			"ip=''; i=0",
			"while [ -z \"$ip\" ] && [ $i -lt 60 ]; do",
			"  ip=$(getent hosts "+id+" </dev/null 2>/dev/null | awk '{ print $1; exit }') || ip=''",
			"  [ -n \"$ip\" ] || ip=$(nslookup "+id+" </dev/null 2>/dev/null | awk '/^Name:/ { n = 1 } n && /^Address/ { a = $NF } END { print a }') || ip=''",
			"  [ -n \"$ip\" ] || { sleep 1; i=$((i + 1)); }",
			"done",
			"[ -n \"$ip\" ] || { echo "+quote("casebox: the service "+svc.Name+" does not resolve on the Daytona link network")+" >&2; exit 1; }",
			"echo \"$ip "+svc.Name+" # casebox-service\" >> /tmp/casebox-hosts",
		)
	}
	lines = append(lines, "cat /tmp/casebox-hosts > /etc/hosts", "rm -f /tmp/casebox-hosts")
	return strings.Join(lines, "\n")
}

// Snapshot saves the sandbox's filesystem as a Daytona snapshot named casebox-snap-<random>.
// Daytona snapshots a container sandbox while it runs when it can; when it answers that the
// sandbox must be stopped, the sandbox is stopped, snapshotted and started again, which ends
// its running processes but keeps its files. Services are not part of the snapshot; a sandbox
// started from it gets fresh ones.
func (p *Provider) Snapshot(ctx context.Context, sb sandbox.Sandbox) (sandbox.SnapshotRef, error) {
	services, err := metaServicesOf(sb)
	if err != nil {
		return sandbox.SnapshotRef{}, err
	}
	if _, err := p.waitState(ctx, sb.ID, "started"); err != nil {
		return sandbox.SnapshotRef{}, err
	}
	name := "casebox-snap-" + randomHex(8)
	target := p.c.platform("/sandbox/" + url.PathEscape(sb.ID) + "/snapshot")
	err = p.c.call(ctx, http.MethodPost, target, sandboxSnapshot{Name: name}, nil)
	stopped := false
	if statusOf(err) == http.StatusBadRequest {
		if err := p.c.call(ctx, http.MethodPost, p.c.platform("/sandbox/"+url.PathEscape(sb.ID)+"/stop"), nil, nil); err != nil {
			return sandbox.SnapshotRef{}, fmt.Errorf("stop the Daytona sandbox %s for its snapshot: %w", sb.ID, err)
		}
		stopped = true
		if _, err = p.waitState(ctx, sb.ID, "stopped"); err == nil {
			err = p.c.call(ctx, http.MethodPost, target, sandboxSnapshot{Name: name}, nil)
		}
	}
	if err == nil {
		err = p.waitSnapshot(ctx, sb.ID, name)
	}
	if stopped {
		if restartErr := p.restart(ctx, sb, services); restartErr != nil {
			return sandbox.SnapshotRef{}, errors.Join(err, restartErr)
		}
	}
	if err != nil {
		return sandbox.SnapshotRef{}, fmt.Errorf("snapshot the Daytona sandbox %s: %w", sb.ID, err)
	}
	return sandbox.SnapshotRef{ID: name, Services: services, Workdir: sb.Workdir}, nil
}

// waitSnapshot waits until the sandbox has left the snapshotting state and the snapshot is
// active. Daytona drops a failed sandbox snapshot without a record, so a snapshot that is
// still missing once the sandbox is done is an error.
func (p *Provider) waitSnapshot(ctx context.Context, id, name string) error {
	for {
		s, err := p.getSandbox(ctx, id)
		if err != nil {
			return err
		}
		busy := s.State == "snapshotting"
		var snap snapshotDTO
		err = p.c.call(ctx, http.MethodGet, p.c.platform("/snapshots/"+url.PathEscape(name)), nil, &snap)
		switch {
		case statusOf(err) == http.StatusNotFound && !busy:
			return fmt.Errorf("daytona finished the snapshot %s without creating it: %s", name, reason(s.ErrorReason))
		case statusOf(err) == http.StatusNotFound:
		case err != nil:
			return fmt.Errorf("read the Daytona snapshot %s: %w", name, err)
		case snap.State == "error" || snap.State == "build_failed":
			return fmt.Errorf("the Daytona snapshot %s failed: %s", name, reason(snap.ErrorReason))
		case snap.State == "active" && !busy:
			return nil
		}
		if err := p.sleep(ctx); err != nil {
			return fmt.Errorf("wait for the Daytona snapshot %s: %w", name, err)
		}
	}
}

// restart starts a sandbox that Snapshot stopped and brings its services back.
func (p *Provider) restart(ctx context.Context, sb sandbox.Sandbox, services []sandbox.Service) error {
	if err := p.c.call(ctx, http.MethodPost, p.c.platform("/sandbox/"+url.PathEscape(sb.ID)+"/start"), nil, nil); err != nil {
		return fmt.Errorf("start the Daytona sandbox %s after its snapshot: %w", sb.ID, err)
	}
	if _, err := p.waitState(ctx, sb.ID, "started"); err != nil {
		return err
	}
	if len(services) > 0 {
		return p.ensureServices(ctx, sb, services)
	}
	return nil
}

func metaServicesOf(sb sandbox.Sandbox) ([]sandbox.Service, error) {
	raw := sb.Meta[metaServices]
	if raw == "" {
		return nil, nil
	}
	var services []sandbox.Service
	if err := json.Unmarshal([]byte(raw), &services); err != nil {
		return nil, fmt.Errorf("the sandbox's service record is damaged: %w", err)
	}
	return services, nil
}

// Destroy deletes the sandbox's services and the sandbox, and waits until Daytona reports
// them gone. A sandbox that is already gone is not an error.
func (p *Provider) Destroy(ctx context.Context, sb sandbox.Sandbox) error {
	linked := map[string]string{}
	if raw := sb.Meta[metaLinked]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &linked)
	}
	var errs []error
	for _, id := range linked {
		errs = append(errs, p.remove(ctx, id))
	}
	errs = append(errs, p.remove(ctx, sb.ID))
	return errors.Join(errs...)
}

func (p *Provider) remove(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	err := p.c.call(ctx, http.MethodDelete, p.c.platform("/sandbox/"+url.PathEscape(id)), nil, nil)
	if err != nil && statusOf(err) != http.StatusNotFound {
		s, getErr := p.getSandbox(ctx, id)
		if getErr == nil && s.State != "destroyed" && s.State != "destroying" {
			return fmt.Errorf("delete the Daytona sandbox %s: %w", id, err)
		}
		if getErr != nil && !errors.Is(getErr, sandbox.ErrNotFound) {
			return fmt.Errorf("delete the Daytona sandbox %s: %w", id, err)
		}
	}
	for {
		s, err := p.getSandbox(ctx, id)
		if errors.Is(err, sandbox.ErrNotFound) || err == nil && s.State == "destroyed" {
			return nil
		}
		if err != nil {
			return err
		}
		if err := p.sleep(ctx); err != nil {
			return fmt.Errorf("wait for the Daytona sandbox %s to be deleted (it is %s): %w", id, s.State, err)
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
