package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox"
)

// ResultsDir is where test commands write their result files.
const ResultsDir = "/results"

// maxResults caps the result files one test command leaves.
const maxResults = 256 << 20

// Patch is a diff to apply in the verifier. Exclude names paths whose changes in it are skipped.
type Patch struct {
	Name    string
	Diff    []byte
	Exclude []string
}

// Result is what a verifier run did. When a patch does not apply, Applied is false, FailedPatch and
// ApplyOutput say which and why, and no command ran: that is an outcome, not an error.
type Result struct {
	Applied     bool
	FailedPatch string
	ApplyOutput string
	Commands    []CommandResult
}

// CommandResult is one test command's run: its exit, its time, the last 40 lines of its output,
// and the files it left in ResultsDir, by path relative to it, for the oracle's parser of Results.
type CommandResult struct {
	Command  string
	Results  string // the format: go-test-json, trx or junit
	ExitCode int
	Duration time.Duration
	TimedOut bool
	Output   string
	Files    map[string][]byte
}

// Verify runs the verifier for an agent's work: a sandbox with no start environment (no model key)
// and the egress allow-list, the pristine base, the agent's diff without its changes to the
// held-out files, then the test patch (the held-out tests), then every test command.
func Verify(ctx context.Context, p sandbox.Provider, spec sandbox.EnvSpec, baseTree io.Reader, agentDiff, testPatch []byte, heldOut []string, commands []repo.TestCommand, egress []string) (Result, error) {
	return ApplyAndRun(ctx, p, spec, baseTree, []Patch{
		{Name: "agent", Diff: agentDiff, Exclude: heldOut},
		{Name: "tests", Diff: testPatch},
	}, commands, egress)
}

// ApplyAndRun starts a sandbox with no start environment, commits the base tree as Seal does,
// applies the patches in order and runs every test command. Validation uses it for each of its
// runs: the base with the test patch, and the source patch with the test patch.
func ApplyAndRun(ctx context.Context, p sandbox.Provider, spec sandbox.EnvSpec, baseTree io.Reader, patches []Patch, commands []repo.TestCommand, egress []string) (Result, error) {
	if err := CheckEgress(egress); err != nil {
		return Result{}, err
	}
	image, err := p.Prepare(ctx, spec)
	if err != nil {
		return Result{}, fmt.Errorf("prepare the environment: %w", err)
	}
	sb, err := p.Start(ctx, image, sandbox.StartOptions{Network: sandbox.NetworkNone, Egress: egress})
	if err != nil {
		return Result{}, fmt.Errorf("start the verifier's sandbox: %w", err)
	}
	defer p.Destroy(context.WithoutCancel(ctx), sb)
	if _, err := commitBase(ctx, p, sb, baseTree, nil); err != nil {
		return Result{}, err
	}
	for _, patch := range patches {
		ok, output, err := apply(ctx, p, sb, patch)
		if err != nil {
			return Result{}, fmt.Errorf("apply %s: %w", patch.Name, err)
		}
		if !ok {
			return Result{FailedPatch: patch.Name, ApplyOutput: output}, nil
		}
	}
	res := Result{Applied: true}
	for _, c := range commands {
		cr, err := runTest(ctx, p, sb, c)
		if err != nil {
			return Result{}, err
		}
		res.Commands = append(res.Commands, cr)
	}
	return res, nil
}

// apply applies one patch to the working tree and the index: exactly first, then with a three-way
// merge from the blobs the patch names. It reports whether the patch applied and what git said.
func apply(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, patch Patch) (bool, string, error) {
	if len(strings.TrimSpace(string(patch.Diff))) == 0 {
		return true, "", nil
	}
	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", `set -e; d=$(mktemp -d); cat >"$d/patch"; echo "$d/patch"`}, Stdin: bytes.NewReader(patch.Diff)})
	if err != nil {
		return false, "", err
	}
	if res.ExitCode != 0 {
		return false, "", fmt.Errorf("copy the patch in: %s", tail(string(res.Stderr), 20))
	}
	file := strings.TrimSpace(string(res.Stdout))
	args := []string{"--whitespace=nowarn"}
	for _, x := range patch.Exclude {
		clean, err := cleanPath(x)
		if err != nil {
			return false, "", err
		}
		args = append(args, "--exclude="+escapeGlob(clean))
	}
	script := gitSafe + ` apply --index "$@" "$0" 2>"$0.exact" && exit 0
` + gitSafe + ` apply --3way "$@" "$0" 2>"$0.3way" && exit 0
cat "$0.exact" "$0.3way" >&2
exit 1`
	res, err = p.Exec(ctx, sb, sandbox.Command{Args: append([]string{"sh", "-c", script, file}, args...), Env: gitEnv})
	if err != nil {
		return false, "", err
	}
	if res.ExitCode != 0 {
		return false, tail(string(res.Stderr), 40), nil
	}
	return true, "", nil
}

// runTest runs one test command as the sandbox user, from an empty ResultsDir (which the
// environment gives the sandbox user), and copies out the files it left there.
func runTest(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, c repo.TestCommand) (CommandResult, error) {
	clear := sandbox.Command{Args: []string{"sh", "-c", `find "$0" -mindepth 1 -delete`, ResultsDir}}
	if err := run(ctx, p, sb, clear, "empty "+ResultsDir); err != nil {
		return CommandResult{}, err
	}
	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", c.Command}, Timeout: c.OrDefault()})
	if err != nil {
		return CommandResult{}, fmt.Errorf("run %q: %w", c.Command, err)
	}
	cr := CommandResult{
		Command:  c.Command,
		Results:  c.Results,
		ExitCode: res.ExitCode,
		Duration: res.Duration,
		TimedOut: res.TimedOut,
		Output:   tail(string(res.Stdout)+string(res.Stderr), 40),
	}
	files, err := copyFiles(ctx, p, sb, ResultsDir, maxResults)
	if err != nil {
		return CommandResult{}, fmt.Errorf("copy the results of %q out: %w", c.Command, err)
	}
	cr.Files = files
	return cr, nil
}

// escapeGlob makes a path a literal pattern for git apply --exclude.
func escapeGlob(p string) string {
	var b strings.Builder
	for _, c := range p {
		if strings.ContainsRune(`*?[\`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}
