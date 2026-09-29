// Package sandbox is the interface every sandbox provider implements (SDD section 12,
// docs/specs/sandboxes.md). Workers run tests and agents only inside sandboxes; the server never
// runs case code. The interface is kept identical to ClaimGate's so both can share one module.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"time"
)

// Provider starts, snapshots and destroys sandboxes.
type Provider interface {
	Prepare(ctx context.Context, spec EnvSpec) (ImageRef, error)
	Start(ctx context.Context, from Ref, opts StartOptions) (Sandbox, error)
	Snapshot(ctx context.Context, sb Sandbox) (SnapshotRef, error)
	Exec(ctx context.Context, sb Sandbox, cmd Command) (ExecResult, error)
	CopyOut(ctx context.Context, sb Sandbox, path string) (io.ReadCloser, error)
	Destroy(ctx context.Context, sb Sandbox) error
}

// ErrNotFound is returned by any call on a sandbox that does not exist or was destroyed.
var ErrNotFound = errors.New("the sandbox does not exist")

// ErrUnsupported is returned, wrapped with the reason, when a provider cannot honour a start
// option, such as an egress allow-list. The conformance suite skips only the checks of such an
// option, and only for this error.
var ErrUnsupported = errors.New("the sandbox provider does not support this")

// User is the uid every sandbox command runs as, unless the runner asks for root.
const User = 10001

// DefaultWorkdir is where the repository goes and commands run.
const DefaultWorkdir = "/workspace"

// OutputCap is the most stdout or stderr an ExecResult holds.
const OutputCap = 16 << 20

// EnvSpec is an environment to build: a base image, install steps run as root on top of the
// context files (the lockfiles those steps read), and the services beside it.
type EnvSpec struct {
	Image    string            // a tag or a digest
	Install  []string          // shell commands, run in Workdir as root at build time
	Context  map[string][]byte // repository-relative path to contents, placed in Workdir before Install
	Services []Service
	Workdir  string // default DefaultWorkdir
}

// Service is an image that runs beside a sandbox, reachable by Name.
type Service struct {
	Name  string
	Image string
	Env   map[string]string
}

// Key identifies an EnvSpec: the lockfile hash of SDD section 7. Equal specs give equal keys.
func (s EnvSpec) Key() string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	write("image", s.Image, "workdir", s.Dir())
	for _, step := range s.Install {
		write("install", step)
	}
	paths := make([]string, 0, len(s.Context))
	for p := range s.Context {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sum := sha256.Sum256(s.Context[p])
		write("file", p, hex.EncodeToString(sum[:]))
	}
	services := append([]Service(nil), s.Services...)
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	for _, svc := range services {
		write("service", svc.Name, svc.Image)
		keys := make([]string, 0, len(svc.Env))
		for k := range svc.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			write("env", k, svc.Env[k])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Dir is the working directory, with its default.
func (s EnvSpec) Dir() string {
	if s.Workdir == "" {
		return DefaultWorkdir
	}
	return s.Workdir
}

// Ref is what a sandbox starts from: an ImageRef or a SnapshotRef.
type Ref interface {
	RefID() string
	RefServices() []Service
	RefWorkdir() string
}

// ImageRef is a prepared environment.
type ImageRef struct {
	ID       string // the provider's name for it
	Key      string // EnvSpec.Key
	Services []Service
	Workdir  string
}

func (r ImageRef) RefID() string          { return r.ID }
func (r ImageRef) RefServices() []Service { return r.Services }
func (r ImageRef) RefWorkdir() string     { return r.Workdir }

// SnapshotRef is a saved sandbox filesystem.
type SnapshotRef struct {
	ID       string
	Services []Service
	Workdir  string
}

func (r SnapshotRef) RefID() string          { return r.ID }
func (r SnapshotRef) RefServices() []Service { return r.Services }
func (r SnapshotRef) RefWorkdir() string     { return r.Workdir }

// Network is what a sandbox may reach.
type Network string

const (
	NetworkNone Network = "none" // the default: nothing outside the sandbox and its services
	NetworkOpen Network = "open"
)

// StartOptions cap a sandbox.
type StartOptions struct {
	Network Network
	// Egress lists the hosts a sandbox with network none may reach, as exact names or "*.domain"
	// wildcards, over HTTPS or HTTP on ports 443 and 80, through a proxy the provider runs beside
	// it; HTTPS_PROXY, HTTP_PROXY and NO_PROXY point every command at it. Nothing else is reachable:
	// no other host, no DNS outside, no raw connection. A provider that cannot honour it returns
	// ErrUnsupported.
	Egress   []string
	CPUs     float64       // default 2
	MemoryMB int           // default 4096
	Lifetime time.Duration // default 2 hours; the provider destroys it after this
	Env      map[string]string
}

// Sandbox is a running sandbox. Meta holds what its provider needs to find it again.
type Sandbox struct {
	ID       string
	Provider string
	Workdir  string
	Meta     map[string]string
}

// Command is one argv to run in a sandbox.
type Command struct {
	Args    []string
	Dir     string // default the sandbox's workdir
	Env     map[string]string
	Stdin   io.Reader
	Timeout time.Duration // default 10 minutes
	Root    bool          // run as root; for the runner only, never for agent commands
}

// ExecResult is what a command did.
type ExecResult struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Truncated bool
	Duration  time.Duration
	TimedOut  bool
}

// Defaults fills unset start options.
func (o StartOptions) Defaults() StartOptions {
	if o.Network == "" {
		o.Network = NetworkNone
	}
	if o.CPUs <= 0 {
		o.CPUs = 2
	}
	if o.MemoryMB <= 0 {
		o.MemoryMB = 4096
	}
	if o.Lifetime <= 0 {
		o.Lifetime = 2 * time.Hour
	}
	return o
}

// TimeoutOrDefault is the command's timeout, with its default.
func (c Command) TimeoutOrDefault() time.Duration {
	if c.Timeout <= 0 {
		return 10 * time.Minute
	}
	return c.Timeout
}
