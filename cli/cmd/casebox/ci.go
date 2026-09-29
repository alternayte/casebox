package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/buildinfo"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// exitError ends the command with a given exit code, after its message.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

// ciRunRef is one CI run the server started (docs/specs/harness-ci.md).
type ciRunRef struct {
	ID           string  `json:"id"`
	Workspace    *string `json:"workspace"`
	Status       string  `json:"status"`
	EvaluationID *string `json:"evaluationId"`
	Message      *string `json:"message"`
}

type ciCase struct {
	CaseID          string `json:"caseId"`
	BaselinePassed  int    `json:"baselinePassed"`
	BaselineRuns    int    `json:"baselineRuns"`
	CandidatePassed int    `json:"candidatePassed"`
	CandidateRuns   int    `json:"candidateRuns"`
	FailedRuns      int    `json:"failedRuns"`
	Regression      bool   `json:"regression"`
}

type ciScored struct {
	Cases    int     `json:"cases"`
	Runs     int     `json:"runs"`
	PassRate float64 `json:"passRate"`
}

type ciRunView struct {
	ID            string              `json:"id"`
	Kind          string              `json:"kind"`
	Workspace     *string             `json:"workspace"`
	Repo          string              `json:"repo"`
	Number        *int                `json:"number"`
	HeadSha       *string             `json:"headSha"`
	Status        string              `json:"status"`
	Message       *string             `json:"message"`
	EvaluationID  *string             `json:"evaluationId"`
	Purpose       *string             `json:"purpose"`
	Repeats       int                 `json:"repeats"`
	Delta         float64             `json:"delta"`
	Estimate      *evaluationEstimate `json:"estimate"`
	SpentUsd      float64             `json:"spentUsd"`
	CapUsd        float64             `json:"capUsd"`
	RunsCompleted int                 `json:"runsCompleted"`
	RunsFailed    int                 `json:"runsFailed"`
	Verdict       *verdictView        `json:"verdict"`
	Scored        *ciScored           `json:"scored"`
	Reason        *string             `json:"reason"`
	Cases         []ciCase            `json:"cases"`
	Baseline      *struct {
		HarnessHash *string    `json:"harnessHash"`
		ScoredAt    *time.Time `json:"scoredAt"`
	} `json:"baseline"`
}

// ciEvent is the part of GitHub's pull_request event casebox ci reads.
type ciEvent struct {
	PullRequest *struct {
		Number int `json:"number"`
		Head   struct {
			Sha string `json:"sha"`
		} `json:"head"`
		Base struct {
			Sha string `json:"sha"`
		} `json:"base"`
	} `json:"pull_request"`
}

func newCICommand() *cobra.Command {
	var baseline bool
	var workerMode, failOn, eventPath string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "Harness CI: a pull request's smoke run, or the nightly baseline (ci token)",
		Long: "Run harness CI in a GitHub Action (docs/specs/harness-ci.md).\n" +
			"On a pull request that changes harness files, the server runs the smoke suite with the pull request's harness and\n" +
			"compares it with the cached baseline of the default branch; the result is posted as a comment on the pull request.\n" +
			"With --baseline (a scheduled run), the server scores the default branch's harness on the approved dev cases whose\n" +
			"score is missing or older than 7 days.\n" +
			"It reads casebox.yml, CASEBOX_SERVER and CASEBOX_TOKEN (a ci token), and GITHUB_EVENT_PATH, GITHUB_REPOSITORY and\n" +
			"GITHUB_SERVER_URL. --worker inline runs this CI run's jobs on this machine's Docker, with the model keys of this\n" +
			"environment. The exit code is 0, or 1 with --fail-on regression when the smoke run found a regression, or 2 when\n" +
			"the request failed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if workerMode != "none" && workerMode != "inline" {
				return exitError{2, fmt.Errorf("--worker %s: use none or inline", workerMode)}
			}
			if failOn != "none" && failOn != "regression" {
				return exitError{2, fmt.Errorf("--fail-on %s: use none or regression", failOn)}
			}
			c, err := newCIRun(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), eventPath)
			if err != nil {
				return exitError{2, err}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return c.run(ctx, baseline, workerMode == "inline", failOn == "regression")
		},
	}
	cmd.Flags().BoolVar(&baseline, "baseline", false, "score the default branch's harness (the scheduled run) instead of a pull request")
	cmd.Flags().StringVar(&workerMode, "worker", "none", "inline: run this CI run's jobs on this machine; none: leave them to the organisation's workers")
	cmd.Flags().StringVar(&failOn, "fail-on", "none", "regression: exit 1 when the smoke run finds a regression")
	cmd.Flags().StringVar(&eventPath, "event", "", "the GitHub event file (default: GITHUB_EVENT_PATH)")
	cmd.Flags().DurationVar(&timeout, "timeout", 90*time.Minute, "how long to follow the CI run")
	return cmd
}

// ciRun is one casebox ci invocation.
type ciRun struct {
	root      string
	cfg       repo.Config
	client    *api.Client
	server    string
	repo      string
	event     ciEvent
	out, log  io.Writer
	interval  time.Duration
	newWorker func(scope string) (*worker.Worker, error)
}

func newCIRun(ctx context.Context, out, log io.Writer, eventPath string) (*ciRun, error) {
	root, cfg, err := repoConfig(ctx)
	if err != nil {
		return nil, err
	}
	server := os.Getenv("CASEBOX_SERVER")
	if server == "" {
		server = cfg.Server
	}
	token := os.Getenv("CASEBOX_TOKEN")
	if server == "" || token == "" {
		return nil, errors.New("set CASEBOX_SERVER (or server in casebox.yml) and CASEBOX_TOKEN to a ci token")
	}
	name := strings.ToLower(os.Getenv("GITHUB_REPOSITORY"))
	if name == "" {
		return nil, errors.New("GITHUB_REPOSITORY is not set; casebox ci runs in a GitHub Action")
	}
	host := "github.com"
	if u, err := url.Parse(os.Getenv("GITHUB_SERVER_URL")); err == nil && u.Host != "" {
		host = strings.ToLower(u.Host)
	}
	c := &ciRun{root: root, cfg: cfg, client: api.New(server, token), server: server, repo: host + "/" + name, out: out, log: log, interval: 10 * time.Second}
	if eventPath == "" {
		eventPath = os.Getenv("GITHUB_EVENT_PATH")
	}
	if eventPath != "" {
		data, err := os.ReadFile(eventPath)
		if err != nil {
			return nil, fmt.Errorf("read the GitHub event: %w", err)
		}
		if err := json.Unmarshal(data, &c.event); err != nil {
			return nil, fmt.Errorf("read the GitHub event: %w", err)
		}
	}
	c.newWorker = func(scope string) (*worker.Worker, error) {
		handlers, err := workerHandlers(ctx, c.client, log)
		if err != nil {
			return nil, err
		}
		host, _ := os.Hostname()
		return &worker.Worker{Client: c.client, ID: fmt.Sprintf("ci-%s-%d-%s", host, os.Getpid(), scope), Version: buildinfo.Version,
			Log: log, Handlers: handlers, Scope: scope, MaxIdle: 5 * time.Second}, nil
	}
	return c, nil
}

// spec is the baseline of casebox.yml: evaluation.baseline with every repository's harness at its
// default branch, and the shared harness at its default branch.
func (c *ciRun) spec() (harnessSpec, error) {
	shared := c.cfg.Harness.Shared
	if c.shared() {
		shared = c.repo
	}
	return compareBaseline(c.cfg.Evaluation.Baseline, shared, "", func() string { return "HEAD" })
}

// shared is true in a shared harness repository: its casebox.yml names no workspace.
func (c *ciRun) shared() bool { return c.cfg.Workspace == "" }

func (c *ciRun) run(ctx context.Context, baseline, inline, failOnRegression bool) error {
	spec, err := c.spec()
	if err != nil {
		return exitError{2, err}
	}
	prices, err := c.cfg.PriceTable()
	if err != nil {
		return exitError{2, err}
	}
	if _, ok := prices[spec.Model]; !ok {
		return exitError{2, cbx.Errorf(cbx.MissingPrice, "casebox.yml has no price for %s; add it under prices", spec.Model)}
	}
	var shared *string
	if c.cfg.Harness.Shared != "" {
		shared = &c.cfg.Harness.Shared
	}

	var started struct {
		Runs []ciRunRef `json:"runs"`
	}
	if baseline {
		if c.shared() {
			return exitError{2, errors.New("a shared harness repository has no baseline of its own; the workspaces that use it score it in their nightly runs")}
		}
		fmt.Fprintf(c.out, "Baseline for workspace %s: %s, model %s, repeats %d.\n", c.cfg.Workspace, spec.Agent, spec.Model, c.cfg.FullRepeats())
		if err := c.client.Do(ctx, http.MethodPost, "/api/v1/ci/baselines", map[string]any{
			"workspace": c.cfg.Workspace, "spec": spec, "repeats": c.cfg.FullRepeats(), "prices": prices,
			"globs": c.cfg.HarnessGlobs(), "shared": shared,
		}, &started); err != nil {
			return exitError{2, fmt.Errorf("request the baseline: %w", err)}
		}
	} else {
		pr := c.event.PullRequest
		if pr == nil {
			return exitError{2, errors.New("the GitHub event is not a pull request; use --baseline for a scheduled run")}
		}
		changed, err := c.changedHarness(ctx, pr.Base.Sha, pr.Head.Sha)
		if err != nil {
			return exitError{2, err}
		}
		if len(changed) == 0 {
			fmt.Fprintln(c.out, "No harness file changed in this pull request; harness CI has nothing to test.")
			return nil
		}
		fmt.Fprintf(c.out, "Harness files changed: %s\n", strings.Join(changed, ", "))
		size, repeats := c.cfg.SmokeSuite()
		body := map[string]any{
			"repo": c.repo, "number": pr.Number, "headSha": pr.Head.Sha, "baseSha": pr.Base.Sha, "spec": spec,
			"onSharedHarness": c.shared(), "size": size, "repeats": repeats, "prices": prices, "serverUrl": c.server,
		}
		if !c.shared() {
			body["workspace"] = c.cfg.Workspace
			body["globs"] = c.cfg.HarnessGlobs()
			body["shared"] = shared
		}
		if err := c.client.Do(ctx, http.MethodPost, "/api/v1/ci/pull-requests", body, &started); err != nil {
			return exitError{2, fmt.Errorf("request harness CI: %w", err)}
		}
	}

	var views []ciRunView
	var mu sync.Mutex
	var wg sync.WaitGroup
	var failed error
	for _, r := range started.Runs {
		if r.Status == "skipped" || r.Status == "failed" {
			fmt.Fprintf(c.out, "%s: %s\n", ciLabel(r.Workspace), deref(r.Message, r.Status))
			if r.Status == "failed" {
				failed = exitError{2, errors.New(deref(r.Message, "the CI run failed"))}
			}
			continue
		}
		wg.Add(1)
		go func(r ciRunRef) {
			defer wg.Done()
			v, err := c.follow(ctx, r, inline)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = err
				return
			}
			views = append(views, v)
		}(r)
	}
	wg.Wait()
	regressions := 0
	for _, v := range views {
		printCIResult(c.out, v)
		for _, cs := range v.Cases {
			if cs.Regression {
				regressions++
			}
		}
	}
	if failed != nil {
		return failed
	}
	if failOnRegression && regressions > 0 {
		return exitError{1, fmt.Errorf("the smoke run found %d regression(s)", regressions)}
	}
	return nil
}

// follow polls one CI run until it ends, with this machine's inline worker when asked.
func (c *ciRun) follow(ctx context.Context, r ciRunRef, inline bool) (ciRunView, error) {
	label := ciLabel(r.Workspace)
	if inline {
		w, err := c.newWorker(r.ID)
		if err != nil {
			return ciRunView{}, exitError{2, err}
		}
		workerCtx, stop := context.WithCancel(ctx)
		defer stop()
		go func() { _ = w.Run(workerCtx) }()
		fmt.Fprintf(c.out, "%s: an inline worker runs this CI run's jobs here.\n", label)
	}
	last := ""
	for {
		var v ciRunView
		err := c.client.Do(ctx, http.MethodGet, "/api/v1/ci/runs/"+url.PathEscape(r.ID), nil, &v)
		switch {
		case err != nil && ctx.Err() != nil:
			return c.timedOut(r, label, inline)
		case err != nil:
			fmt.Fprintf(c.log, "%s: reading the CI run failed: %v\n", label, err)
		default:
			line := fmt.Sprintf("%s: %s, %d runs complete, %d failed to run, %s spent", label, v.Status, v.RunsCompleted, v.RunsFailed, usd(v.SpentUsd))
			if v.Estimate != nil {
				line += " of " + usd(v.Estimate.TotalUsd) + " estimated"
			}
			if line != last {
				fmt.Fprintln(c.out, line)
				last = line
			}
			switch v.Status {
			case "done", "cancelled", "skipped":
				return v, nil
			case "failed":
				return v, exitError{2, errors.New(deref(v.Message, "the CI run failed"))}
			case "waiting":
				fmt.Fprintf(c.out, "%s: the estimate is above the confirmation threshold; a Member confirms it on the evaluation's page. The result comes as a comment.\n", label)
				return v, nil
			}
		}
		select {
		case <-ctx.Done():
			return c.timedOut(r, label, inline)
		case <-time.After(c.interval):
		}
	}
}

// timedOut ends following at --timeout. The organisation's workers finish the run and the comment
// still comes; an inline worker's run cannot finish without this machine.
func (c *ciRun) timedOut(r ciRunRef, label string, inline bool) (ciRunView, error) {
	if inline {
		return ciRunView{}, exitError{2, fmt.Errorf("%s: the CI run did not finish before --timeout; raise it", label)}
	}
	fmt.Fprintf(c.out, "%s: still running when --timeout ended; the result comes as a comment on the pull request.\n", label)
	return ciRunView{ID: r.ID, Workspace: r.Workspace, Status: "running"}, nil
}

func ciLabel(workspace *string) string {
	if workspace == nil || *workspace == "" {
		return "CI run"
	}
	return "Workspace " + *workspace
}

// changedHarness lists the files the pull request changed that the harness globs match (casebox.yml
// at the head, the checkout). The base and head commits are fetched when the checkout lacks them.
func (c *ciRun) changedHarness(ctx context.Context, base, head string) ([]string, error) {
	if base == "" || head == "" {
		return nil, errors.New("the pull request event names no base or head commit")
	}
	diff := func() (string, error) {
		out, err := exec.CommandContext(ctx, "git", "-C", c.root, "diff", "--name-only", "-z", base, head, "--").Output()
		return string(out), err
	}
	out, err := diff()
	if err != nil {
		if ferr := exec.CommandContext(ctx, "git", "-C", c.root, "fetch", "--quiet", "--no-tags", "origin", base, head).Run(); ferr != nil {
			return nil, fmt.Errorf("fetch the pull request's base and head commits: %w", ferr)
		}
		if out, err = diff(); err != nil {
			return nil, fmt.Errorf("list the pull request's changed files: %w", err)
		}
	}
	var changed []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" && repo.MatchesAny(c.cfg.HarnessGlobs(), f) {
			changed = append(changed, f)
		}
	}
	return changed, nil
}

func printCIResult(out io.Writer, v ciRunView) {
	label := ciLabel(v.Workspace)
	switch {
	case v.Scored != nil:
		fmt.Fprintf(out, "%s: baseline scored: %d cases, %d runs, %.1f%% passed. Spent %s.\n", label, v.Scored.Cases, v.Scored.Runs, v.Scored.PassRate*100, usd(v.SpentUsd))
		return
	case v.Verdict == nil:
		fmt.Fprintf(out, "%s: no verdict (%s). %s\n", label, v.Status, deref(v.Message, ""))
		return
	}
	regressions := 0
	for _, cs := range v.Cases {
		if cs.Regression {
			regressions++
		}
	}
	if regressions == 0 {
		fmt.Fprintf(out, "%s: no regression found. No case the baseline passed in every run failed with this change.\n", label)
	} else {
		fmt.Fprintf(out, "%s: %d regression(s): the baseline passed these cases in every run, the candidate failed them in every run.\n", label, regressions)
	}
	vv := *v.Verdict
	word := vv.Verdict
	switch deref(vv.Reason, "") {
	case "smoke":
		word = "inconclusive (smoke run)"
	case noDifference:
		word = "inconclusive, no difference detected"
	}
	fmt.Fprintf(out, "  Verdict: %s\n", word)
	fmt.Fprintf(out, "  Δ pass rate (candidate − baseline): %s, %s interval %s, %d cases, %d runs\n", points(vv.Delta), levelText(vv.Level), intervalPoints(vv.Lower, vv.Upper), vv.Cases, vv.Runs)
	if v.Estimate != nil {
		fmt.Fprintf(out, "  Detects:     %s\n", detectableText(v.Estimate.DetectableEffect))
	}
	fmt.Fprintf(out, "  Cost:        %s spent; %d runs failed to run\n", usd(v.SpentUsd), v.RunsFailed)
	if v.Baseline != nil && v.Baseline.ScoredAt != nil {
		fmt.Fprintf(out, "  Baseline:    the default branch's harness %s, scored %s\n", shortHash(deref(v.Baseline.HarnessHash, "")), v.Baseline.ScoredAt.UTC().Format("2006-01-02"))
	}
	fmt.Fprintln(out, "  Case                      Baseline  Candidate")
	for _, cs := range v.Cases {
		mark := ""
		if cs.Regression {
			mark = "  regression"
		} else if cs.FailedRuns > 0 {
			mark = fmt.Sprintf("  %d failed to run", cs.FailedRuns)
		}
		fmt.Fprintf(out, "  %-24s  %d/%-6d  %d/%d%s\n", cs.CaseID, cs.BaselinePassed, cs.BaselineRuns, cs.CandidatePassed, cs.CandidateRuns, mark)
	}
	if v.HeadSha != nil {
		fmt.Fprintf(out, "A smoke run does not claim better. For a full comparison: casebox compare --candidate harness=%s\n", *v.HeadSha)
	}
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
