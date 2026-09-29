package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Recipe is the environment block of casebox.yml (docs/specs/sandboxes.md): one per workspace,
// confirmed by a person before any case is mined.
type Recipe struct {
	Image     string             `yaml:"image" json:"image"`
	Install   []string           `yaml:"install,omitempty" json:"install"`
	Lockfiles []string           `yaml:"lockfiles,omitempty" json:"lockfiles"`
	Test      []TestCommand      `yaml:"test" json:"test"`
	Services  map[string]Service `yaml:"services,omitempty" json:"services"`
	Links     []string           `yaml:"links,omitempty" json:"links"`
}

// TestCommand is one test run: a shell command, the format of its results, and its timeout.
type TestCommand struct {
	Command string   `yaml:"command" json:"command"`
	Results string   `yaml:"results,omitempty" json:"results,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Service is a container the tests need, such as Postgres.
type Service struct {
	Image string            `yaml:"image" json:"image"`
	Env   map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// ResultFormats are the test result formats step 9's parsers read.
var ResultFormats = map[string]bool{"go-test-json": true, "trx": true, "junit": true}

// DefaultTestTimeout applies to a test command without its own.
const DefaultTestTimeout = 10 * time.Minute

// Duration reads "10m" or "90s" in YAML and JSON.
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("%q is not a duration such as 10m", text)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// OrDefault is the timeout, or the default when unset.
func (t TestCommand) OrDefault() time.Duration {
	if t.Timeout <= 0 {
		return DefaultTestTimeout
	}
	return time.Duration(t.Timeout)
}

// Validate says what is missing or wrong.
func (r Recipe) Validate() error {
	if r.Image == "" {
		return fmt.Errorf("the recipe names no image")
	}
	if len(r.Test) == 0 {
		return fmt.Errorf("the recipe names no test command")
	}
	for _, t := range r.Test {
		if t.Command == "" {
			return fmt.Errorf("a test entry has no command")
		}
		if t.Results != "" && !ResultFormats[t.Results] {
			return fmt.Errorf("test results %q: use go-test-json, trx or junit", t.Results)
		}
	}
	for name, s := range r.Services {
		if s.Image == "" {
			return fmt.Errorf("service %s names no image", name)
		}
	}
	return nil
}

// JSON is the canonical form the server hashes: empty lists are [], and maps are sorted by key.
func (r Recipe) JSON() json.RawMessage {
	c := r
	if c.Install == nil {
		c.Install = []string{}
	}
	if c.Lockfiles == nil {
		c.Lockfiles = []string{}
	}
	if c.Test == nil {
		c.Test = []TestCommand{}
	}
	if c.Services == nil {
		c.Services = map[string]Service{}
	}
	if c.Links == nil {
		c.Links = []string{}
	}
	data, _ := json.Marshal(c)
	return data
}

// TrackedFiles lists the files of rev in the repository in dir, a checkout or a bare mirror.
func TrackedFiles(ctx context.Context, dir, rev string) ([]string, error) {
	out, err := git(ctx, dir, "-c", "core.quotePath=false", "ls-tree", "-r", "-z", "--name-only", "--full-tree", rev)
	if err != nil {
		return nil, fmt.Errorf("list the files of %s: %w", rev, err)
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// ReadAt reads one file of rev.
func ReadAt(ctx context.Context, dir, rev, file string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", rev+":"+file)
	cmd.Dir = dir
	return cmd.Output()
}

// Archive streams the tree of rev as a tar, without Casebox's own files (.casebox/). The caller
// closes the stream; Close reports whether git succeeded.
func Archive(ctx context.Context, dir, rev string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", rev, "--", ".", ":(exclude).casebox")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &archive{ReadCloser: out, cmd: cmd, stderr: &stderr}, nil
}

type archive struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
}

func (a *archive) Close() error {
	_, _ = io.Copy(io.Discard, a.ReadCloser)
	if err := a.cmd.Wait(); err != nil {
		return fmt.Errorf("git archive: %v: %s", err, strings.TrimSpace(a.stderr.String()))
	}
	return nil
}

// Resolve returns the commit a revision names.
func Resolve(ctx context.Context, dir, rev string) (string, error) {
	sha, err := git(ctx, dir, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s names no commit", rev)
	}
	return sha, nil
}
