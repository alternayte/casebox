package recipe

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox"
)

// Test outcomes in a report.
const (
	Passed   = "passed"
	Failed   = "failed"
	TimedOut = "timed_out"
)

// TestReport is what one test command did.
type TestReport struct {
	Command  string  `json:"command"`
	Status   string  `json:"status"`
	ExitCode int     `json:"exitCode"`
	Seconds  float64 `json:"seconds"`
	Tail     string  `json:"tail,omitempty"` // the last 40 lines of output, when it did not pass
}

// Report is the result of a check: the build, then every test command.
type Report struct {
	Hash         string       `json:"hash"`
	Commit       string       `json:"commit"`
	Provider     string       `json:"provider"`
	Key          string       `json:"key"`
	BuildSeconds float64      `json:"buildSeconds"`
	BuildError   string       `json:"buildError,omitempty"`
	Tests        []TestReport `json:"tests"`
}

// Passed is true when the environment built and every test command passed.
func (r Report) Passed() bool {
	if r.BuildError != "" || len(r.Tests) == 0 {
		return false
	}
	for _, t := range r.Tests {
		if t.Status != Passed {
			return false
		}
	}
	return true
}

// Spec turns a recipe into an environment: its image, install steps and services, with the
// repository files its lockfile globs match as the build context.
func Spec(r repo.Recipe, files []string, read func(path string) ([]byte, error)) (sandbox.EnvSpec, error) {
	spec := sandbox.EnvSpec{Image: r.Image, Install: r.Install, Context: map[string][]byte{}}
	for _, f := range files {
		for _, g := range r.Lockfiles {
			if repo.MatchGlob(g, f) {
				data, err := read(f)
				if err != nil {
					return spec, fmt.Errorf("read lockfile %s: %w", f, err)
				}
				spec.Context[f] = data
				break
			}
		}
	}
	names := make([]string, 0, len(r.Services))
	for name := range r.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := r.Services[name]
		spec.Services = append(spec.Services, sandbox.Service{Name: name, Image: s.Image, Env: s.Env})
	}
	return spec, nil
}

// Check builds the environment, copies the repository tree (a tar stream) into a sandbox, and
// runs every test command with its timeout. The sandbox has an open network: tests that fetch
// at run time fail later in sealed runs, and the report is where a person sees that first.
func Check(ctx context.Context, p sandbox.Provider, name string, r repo.Recipe, spec sandbox.EnvSpec, tree io.Reader) (Report, error) {
	report := Report{Provider: name, Key: spec.Key(), Tests: []TestReport{}}
	start := time.Now()
	image, err := p.Prepare(ctx, spec)
	report.BuildSeconds = time.Since(start).Seconds()
	if err != nil {
		report.BuildError = err.Error()
		return report, nil
	}
	sb, err := p.Start(ctx, image, sandbox.StartOptions{Network: sandbox.NetworkOpen})
	if err != nil {
		return report, fmt.Errorf("start a sandbox: %w", err)
	}
	defer p.Destroy(context.WithoutCancel(ctx), sb)

	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"tar", "-x", "-f", "-", "-C", sb.Workdir}, Stdin: tree})
	if err != nil {
		return report, fmt.Errorf("copy the repository in: %w", err)
	}
	if res.ExitCode != 0 {
		return report, fmt.Errorf("copy the repository in: %s", strings.TrimSpace(string(res.Stderr)))
	}

	for _, t := range r.Test {
		res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", t.Command}, Timeout: t.OrDefault()})
		if err != nil {
			return report, fmt.Errorf("run %q: %w", t.Command, err)
		}
		tr := TestReport{Command: t.Command, ExitCode: res.ExitCode, Seconds: res.Duration.Seconds(), Status: Passed}
		switch {
		case res.TimedOut:
			tr.Status = TimedOut
		case res.ExitCode != 0:
			tr.Status = Failed
		}
		if tr.Status != Passed {
			tr.Tail = tail(string(res.Stdout)+string(res.Stderr), 40)
		}
		report.Tests = append(report.Tests, tr)
	}
	return report, nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
