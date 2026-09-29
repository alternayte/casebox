package evaluate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/agents"
	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/cases"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/runner"
	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// RunPayload is the payload of a run job, as the server's evaluation workflow writes it.
type RunPayload struct {
	EvaluationID string                `json:"evaluationId"`
	RunID        string                `json:"runId"`
	CaseID       string                `json:"caseId"`
	Side         string                `json:"side"`
	Repeat       int                   `json:"repeat"`
	Spec         agents.Spec           `json:"spec"`
	Recipe       repo.Recipe           `json:"recipe"`
	RecipeHash   string                `json:"recipeHash"`
	Repos        []cases.CaseRepo      `json:"repos"`
	Instruction  string                `json:"instruction"`
	Oracle       string                `json:"oracle"`
	Prices       map[string]repo.Price `json:"prices"`
}

// RunAnswer is the answer to a run job (the server's RunAnswer record).
type RunAnswer struct {
	RunID            string         `json:"runId"`
	Diff             *string        `json:"diff"`
	Log              *string        `json:"log"`
	Trace            *string        `json:"trace"`
	Usage            *Usage         `json:"usage"`
	CostUSD          float64        `json:"costUsd"`
	Seconds          float64        `json:"seconds"`
	Turns            int            `json:"turns"`
	ToolCalls        int            `json:"toolCalls"`
	Model            *string        `json:"model"`
	TimedOut         bool           `json:"timedOut"`
	TokenCapExceeded bool           `json:"tokenCapExceeded"`
	ProcessChecks    *ProcessChecks `json:"processChecks"`
	HarnessHash      *string        `json:"harnessHash"`
}

// Usage is a run's tokens by kind, and the cost the agent reported, if it did.
type Usage struct {
	InputTokens      int64    `json:"inputTokens"`
	OutputTokens     int64    `json:"outputTokens"`
	CacheReadTokens  int64    `json:"cacheReadTokens"`
	CacheWriteTokens int64    `json:"cacheWriteTokens"`
	CostUSD          *float64 `json:"costUsd"`
}

// ProcessChecks are what the trace shows about how the agent worked.
type ProcessChecks struct {
	RanTestsBeforeDone     bool `json:"ranTestsBeforeDone"`
	EditedTestAfterFailure bool `json:"editedTestAfterFailure"`
}

// Trace is the trace blob: the run's canonical events.
type Trace struct {
	Events []capture.Event `json:"events"`
}

// Run runs a run job: one side's agent on one case, in a sealed sandbox.
func (j Jobs) Run(ctx context.Context, job worker.Job) (any, error) {
	var p RunPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.RunID == "" || p.CaseID == "" || len(p.Repos) == 0 {
		return nil, worker.Permanent{Err: errors.New("the job names no run, case or repository")}
	}
	return run(ctx, j.deps(), p)
}

// maxLog caps the session log copied out of the sandbox, and maxStdout the agent's stdout.
const (
	maxLog    = 512 << 20
	maxStdout = 512 << 20
)

func run(ctx context.Context, d deps, pl RunPayload) (RunAnswer, error) {
	a, err := agents.For(pl.Spec)
	if err != nil {
		return RunAnswer{}, worker.Permanent{Err: fmt.Errorf("the %s side's spec: %w", pl.Side, err)}
	}
	if err := pl.Recipe.Validate(); err != nil {
		return RunAnswer{}, worker.Permanent{Err: err}
	}
	// The price is checked before the agent spends anything: a run whose cost cannot be counted
	// would break the evaluation's budget.
	price, ok := pl.Prices[pl.Spec.Model]
	if !ok {
		return RunAnswer{}, worker.Permanent{Err: fmt.Errorf("the price table has no price for the model %s", pl.Spec.Model)}
	}

	env, hosts, err := agentAccess(a, d.env)
	if err != nil {
		return RunAnswer{}, err
	}

	sealed, err := openSealed(ctx, d, pl.Repos)
	if err != nil {
		return RunAnswer{}, err
	}
	o, err := readOracle(ctx, d, pl.Oracle)
	if err != nil {
		return RunAnswer{}, err
	}
	overlay, err := selectHarness(ctx, sealed, pl.Spec.Harness)
	if err != nil {
		return RunAnswer{}, err
	}
	denied, err := deniedPackages(ctx, d, pl.Repos)
	if err != nil {
		return RunAnswer{}, err
	}

	tree, files, err := baseTree(ctx, sealed)
	if err != nil {
		return RunAnswer{}, err
	}
	defer os.Remove(tree.Name())
	defer tree.Close()
	spec, err := envSpec(ctx, pl.Recipe, sealed, files)
	if err != nil {
		return RunAnswer{}, err
	}
	// The agent's install steps follow the recipe's, so the image key includes the agent and its
	// version.
	spec.Install = append(append([]string(nil), spec.Install...), a.Install()...)

	p := d.provider
	sb, err := runner.Seal(ctx, p, spec, tree, o.TestFiles, hosts, &denied, env)
	if err != nil {
		if errors.Is(err, sandbox.ErrUnsupported) {
			return RunAnswer{}, worker.Permanent{Err: fmt.Errorf("the sandbox provider cannot close the agent's network to the model API and the registry mirror, and an agent never runs with open network: %w", err)}
		}
		return RunAnswer{}, err
	}
	defer p.Destroy(context.WithoutCancel(ctx), sb)

	sb, err = runner.Overlay(ctx, p, sb, overlay.files, overlay.globs, overlay.none)
	if err != nil {
		return RunAnswer{}, fmt.Errorf("overlay the %s harness: %w", pl.Spec.Harness, err)
	}

	workdir := sb.Workdir
	if workdir == "" {
		workdir = spec.Dir()
	}
	home, runDir, err := prepareRun(ctx, p, sb, pl.Instruction)
	if err != nil {
		return RunAnswer{}, err
	}
	var limit int64
	if c := pl.Spec.Settings.TokenCap; c != nil {
		limit = *c
	}
	out, err := runAgent(ctx, p, sb, a, a.Argv(runDir+"/instruction.md", workdir), runDir+"/stdout", pl.Spec.Settings.Timeout(), limit)
	if err != nil {
		return RunAnswer{}, err
	}

	diff, err := runner.AgentDiff(ctx, p, sb)
	if err != nil {
		return RunAnswer{}, err
	}
	var log []byte
	if glob := a.LogGlob(home); glob != "" {
		if log, err = sessionLog(ctx, p, sb, glob); err != nil {
			return RunAnswer{}, err
		}
	}
	// A log the parser cannot read is the agent's outcome, not the worker's failure: the run is
	// recorded without its metrics.
	metrics, perr := a.Parse(log, out.stdout)
	if perr != nil {
		metrics = agents.Metrics{}
	}
	usage := metrics.Usage
	if agents.Tokens(usage) == 0 && usage.CacheReadTokens == 0 {
		usage = out.watched
	}
	capped := out.capped || (limit > 0 && agents.Tokens(usage) > limit)

	var commands []string
	for _, c := range pl.Recipe.Test {
		commands = append(commands, c.Command)
	}
	ranTests, editedTests := agents.ProcessChecks(metrics.Events, workdir, commands, repo.DefaultTestGlobs)

	answer := RunAnswer{
		RunID:            pl.RunID,
		Usage:            &Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens, CostUSD: usage.CostUSD},
		CostUSD:          Cost(usage, price),
		Seconds:          out.seconds,
		Turns:            metrics.Turns,
		ToolCalls:        metrics.ToolCalls,
		TimedOut:         out.timedOut,
		TokenCapExceeded: capped,
		ProcessChecks:    &ProcessChecks{RanTestsBeforeDone: ranTests, EditedTestAfterFailure: editedTests},
		HarnessHash:      &overlay.hash,
	}
	model := metrics.Model
	if model == "" {
		model = pl.Spec.Model
	}
	answer.Model = &model

	if len(diff) > maxBlob {
		return RunAnswer{}, worker.Permanent{Err: fmt.Errorf("the agent's diff holds %d MiB; a blob holds at most %d MiB", len(diff)>>20, maxBlob>>20)}
	}
	diffHash, err := d.put(ctx, "text/x-diff", diff)
	if err != nil {
		return RunAnswer{}, fmt.Errorf("upload the diff: %w", err)
	}
	answer.Diff = &diffHash
	if log != nil {
		logHash, err := d.put(ctx, "application/x-ndjson", logTail(log, maxBlob))
		if err != nil {
			return RunAnswer{}, fmt.Errorf("upload the session log: %w", err)
		}
		answer.Log = &logHash
	}
	events := metrics.Events
	if events == nil {
		events = []capture.Event{}
	}
	trace, err := json.Marshal(Trace{Events: events})
	if err != nil {
		return RunAnswer{}, err
	}
	if len(trace) > maxBlob {
		return RunAnswer{}, worker.Permanent{Err: fmt.Errorf("the trace holds %d MiB; a blob holds at most %d MiB", len(trace)>>20, maxBlob>>20)}
	}
	traceHash, err := d.put(ctx, "application/json", trace)
	if err != nil {
		return RunAnswer{}, fmt.Errorf("upload the trace: %w", err)
	}
	answer.Trace = &traceHash
	return answer, nil
}

// agentAccess is the environment the agent's sandbox starts with and the model API hosts it may
// reach. The command agent may run without any key: then it gets neither.
func agentAccess(a agents.Adapter, workerEnv map[string]string) (map[string]string, []string, error) {
	if a.Spec().Agent == agents.CommandCLI {
		keyed, err := a.Hosts(workerEnv)
		if err != nil {
			return nil, nil, err
		}
		if len(keyed) == 0 {
			return map[string]string{}, nil, nil
		}
	}
	env, err := a.Env(workerEnv)
	if err != nil {
		// Another worker may hold the key: the job is retried.
		return nil, nil, err
	}
	hosts, err := a.Hosts(env)
	if err != nil {
		return nil, nil, err
	}
	if err := runner.CheckEgress(hosts); err != nil {
		return nil, nil, worker.Permanent{Err: err}
	}
	return env, hosts, nil
}

// Cost is the run's cost in USD: the agent's own figure when it reports one, else the tokens
// times the model's price (USD per million tokens), where cache reads and cache writes without a
// price of their own cost as input.
func Cost(u capture.Usage, p repo.Price) float64 {
	if u.CostUSD != nil {
		return *u.CostUSD
	}
	val := func(v *float64, fallback float64) float64 {
		if v == nil {
			return fallback
		}
		return *v
	}
	input := val(p.Input, 0)
	return (float64(u.InputTokens)*input +
		float64(u.OutputTokens)*val(p.Output, 0) +
		float64(u.CacheReadTokens)*val(p.CacheRead, input) +
		float64(u.CacheWriteTokens)*val(p.CacheWrite, input)) / 1e6
}

// harnessOverlay is a side's harness over the sealed repositories: the files to write (with the
// layout prefix), the globs whose base files it replaces, and its files hash.
type harnessOverlay struct {
	files map[string][]byte
	globs []string
	none  bool
	hash  string
}

// selectHarness reads the side's harness. none removes the files the globs of casebox.yml at the
// default branch match (the default list without one). A ref is resolved in each sealed
// repository's mirror, and the harness files its casebox.yml globs (or the default list) match
// at that commit are overlaid. The hash is repo.Harness's for one repository, and a hash of each
// repository's for several.
func selectHarness(ctx context.Context, sealed []sealedRepo, ref string) (harnessOverlay, error) {
	h := harnessOverlay{files: map[string][]byte{}, none: ref == "none"}
	if strings.HasPrefix(ref, "-") || strings.TrimSpace(ref) == "" {
		return harnessOverlay{}, worker.Permanent{Err: fmt.Errorf("the harness %q is not a git ref or none", ref)}
	}
	var hashes []string
	for _, r := range sealed {
		rev := "HEAD"
		if !h.none {
			commit, err := repo.Resolve(ctx, r.dir, ref)
			if err != nil {
				return harnessOverlay{}, worker.Permanent{Err: fmt.Errorf("the harness ref %q names no commit in %s", ref, r.Repo)}
			}
			rev = commit
		}
		globs := configAt(ctx, r.dir, rev).HarnessGlobs()
		for _, g := range globs {
			h.globs = append(h.globs, r.prefix+g)
		}
		hash := emptyHarness
		if !h.none {
			found, err := repo.Harness(ctx, r.dir, rev, globs)
			if err != nil {
				return harnessOverlay{}, err
			}
			for _, f := range found.Files {
				body, err := repo.ReadAt(ctx, r.dir, rev, f)
				if err != nil {
					return harnessOverlay{}, fmt.Errorf("read %s of %s at %s: %w", f, r.Repo, short(rev), err)
				}
				h.files[r.prefix+f] = body
			}
			hash = found.Hash
		}
		hashes = append(hashes, r.prefix+" "+hash)
	}
	if len(sealed) == 1 {
		h.hash = strings.TrimPrefix(hashes[0], " ")
	} else {
		sort.Strings(hashes)
		sum := sha256.Sum256([]byte(strings.Join(hashes, "\n") + "\n"))
		h.hash = hex.EncodeToString(sum[:])
	}
	return h, nil
}

// emptyHarness is repo.Harness's hash of no files: the harness none.
var emptyHarness = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

// deniedPackages reads the manifests of every workspace repository of the case at its default
// branch, and of each sealed repository at its base too, for the registry mirror to refuse.
func deniedPackages(ctx context.Context, d deps, repos []cases.CaseRepo) (sandbox.Mirror, error) {
	files := map[string][]byte{}
	read := func(key, dir, rev string) error {
		tracked, err := repo.TrackedFiles(ctx, dir, rev)
		if err != nil {
			return err
		}
		for _, f := range tracked {
			if !runner.IsManifest(f) {
				continue
			}
			body, err := repo.ReadAt(ctx, dir, rev, f)
			if err != nil {
				return fmt.Errorf("read %s: %w", f, err)
			}
			files[key+"/"+f] = body
		}
		return nil
	}
	for i, r := range repos {
		dir, err := d.open(ctx, r.Repo)
		if err != nil {
			return sandbox.Mirror{}, err
		}
		if err := read("r"+strconv.Itoa(i), dir, "HEAD"); err != nil {
			return sandbox.Mirror{}, fmt.Errorf("the manifests of %s: %w", r.Repo, err)
		}
		if r.Role == cases.RoleSealed {
			if err := read("r"+strconv.Itoa(i)+"-base", dir, r.Base); err != nil {
				return sandbox.Mirror{}, fmt.Errorf("the manifests of %s at the base: %w", r.Repo, err)
			}
		}
	}
	return runner.DeniedPackages(files), nil
}

// prepareRun writes the instruction to a fresh folder outside the working directory and finds the
// sandbox user's home, where agents keep their session logs.
func prepareRun(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, instruction string) (home, dir string, err error) {
	res, err := p.Exec(ctx, sb, sandbox.Command{
		Args:  []string{"sh", "-c", `set -e; d=$(mktemp -d /tmp/casebox-run.XXXXXX); cat >"$d/instruction.md"; printf '%s\n%s\n' "$HOME" "$d"`},
		Stdin: strings.NewReader(instruction),
	})
	if err != nil {
		return "", "", fmt.Errorf("write the instruction: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	if res.ExitCode != 0 || len(lines) != 2 || lines[1] == "" {
		return "", "", fmt.Errorf("write the instruction: exit %d: %s", res.ExitCode, clip(string(res.Stderr), 2000))
	}
	home = lines[0]
	if home == "" || home == "/" {
		return "", "", errors.New("the sandbox user has no home, where agents keep their session logs")
	}
	return home, lines[1], nil
}

// agentOutcome is what running the agent did.
type agentOutcome struct {
	stdout   []byte
	seconds  float64
	timedOut bool
	capped   bool
	watched  capture.Usage // the usage its stdout showed
}

// pollEvery is how often the cap watcher reads the agent's stdout while it runs.
const pollEvery = 2 * time.Second

// runAgent runs argv with its timeout, its stdout in the file stdout. When the agent streams
// usage and there is a cap, the stdout is read while it runs and the agent is stopped when its
// tokens pass the cap; otherwise the cap is checked on the whole stdout afterwards.
func runAgent(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, a agents.Adapter, argv []string, stdout string, timeout time.Duration, limit int64) (agentOutcome, error) {
	cmd := sandbox.Command{
		Args:    append([]string{"sh", "-c", `f=$1; shift; exec "$@" >"$f"`, "sh", stdout}, argv...),
		Timeout: timeout,
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	watching := a.StreamsUsage() && limit > 0
	var (
		watched  = make(chan agents.CapResult, 1)
		finished = make(chan struct{})
		tailed   = make(chan struct{})
	)
	if watching {
		pr, pw := io.Pipe()
		go func() {
			res, _ := a.WatchCap(pr, limit)
			pr.CloseWithError(io.ErrClosedPipe)
			if res.Exceeded {
				stop()
			}
			watched <- res
		}()
		go func() {
			defer close(tailed)
			tail(ctx, p, sb, stdout, pw, finished)
		}()
	}

	start := time.Now()
	res, err := p.Exec(runCtx, sb, cmd)
	seconds := time.Since(start).Seconds()
	close(finished)
	out := agentOutcome{seconds: seconds}
	if watching {
		<-tailed
		w := <-watched
		out.capped, out.watched = w.Exceeded, w.Usage
	}
	switch {
	case out.capped:
		// Cancelling the call stopped the agent: the provider kills every process of the command,
		// its children included, as it does at a timeout.
	case err != nil:
		return agentOutcome{}, fmt.Errorf("run the agent: %w", err)
	default:
		out.timedOut = res.TimedOut
	}

	body, err := copyFile(ctx, p, sb, stdout, maxStdout)
	if err != nil {
		// An agent that never started leaves no stdout; its outcome is an empty run.
		body = nil
	}
	out.stdout = body
	if !watching {
		w, _ := a.WatchCap(bytes.NewReader(body), limit)
		out.capped, out.watched = w.Exceeded, w.Usage
	}
	return out, nil
}

// tail copies the growing file into w every pollEvery until finished closes, then copies the rest
// and closes w. It stops early when w refuses more.
func tail(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, file string, w *io.PipeWriter, finished <-chan struct{}) {
	var offset int64
	read := func() bool {
		res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", `test -f "$0" && tail -c +"$1" "$0"`, file, strconv.FormatInt(offset+1, 10)}, Timeout: time.Minute})
		if err != nil || res.ExitCode != 0 || len(res.Stdout) == 0 {
			return err == nil
		}
		offset += int64(len(res.Stdout))
		_, werr := w.Write(res.Stdout)
		return werr == nil
	}
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-finished:
			read()
			w.Close()
			return
		case <-ticker.C:
			if !read() {
				w.Close()
				<-finished
				return
			}
		}
	}
}

// sessionLog copies out the newest file the log glob matches; an agent that wrote none has none.
func sessionLog(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, glob string) ([]byte, error) {
	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", `set -- $1; for f do shift; [ -f "$f" ] && set -- "$@" "$f"; done; [ "$#" -gt 0 ] || exit 0; ls -1t -- "$@" | head -n 1`, "sh", glob}})
	if err != nil {
		return nil, fmt.Errorf("find the session log: %w", err)
	}
	file := strings.TrimSpace(string(res.Stdout))
	if res.ExitCode != 0 || file == "" {
		return nil, nil
	}
	body, err := copyFile(ctx, p, sb, file, maxLog)
	if err != nil {
		return nil, fmt.Errorf("copy the session log out: %w", err)
	}
	return body, nil
}

// logTail keeps the end of a log that exceeds limit, from a line start.
func logTail(log []byte, limit int) []byte {
	if len(log) <= limit {
		return log
	}
	cut := log[len(log)-limit:]
	if i := bytes.IndexByte(cut, '\n'); i >= 0 {
		return cut[i+1:]
	}
	return cut
}
