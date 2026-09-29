package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/repo"
)

// The JSON of server/Casebox.Server/Features/Evaluations (docs/specs/evaluations.md, API).

type agentSettings struct {
	MaxTurns       *int   `json:"maxTurns"`
	TimeoutMinutes *int   `json:"timeoutMinutes"`
	TokenCap       *int64 `json:"tokenCap"`
}

type commandTemplate struct {
	Template  string  `json:"template"`
	LogGlob   *string `json:"logGlob"`
	LogFormat *string `json:"logFormat"`
}

// harnessSpec is one side of an evaluation.
type harnessSpec struct {
	Agent        string           `json:"agent"`
	AgentVersion string           `json:"agentVersion"`
	Model        string           `json:"model"`
	Effort       *string          `json:"effort"`
	Harness      string           `json:"harness"`
	Settings     agentSettings    `json:"settings"`
	Command      *commandTemplate `json:"command"`
}

type evaluationRequest struct {
	Workspace string                `json:"workspace"`
	Baseline  harnessSpec           `json:"baseline"`
	Candidate harnessSpec           `json:"candidate"`
	MaxCases  *int                  `json:"maxCases,omitempty"`
	Repeats   int                   `json:"repeats"`
	Delta     float64               `json:"delta"`
	CapUsd    *float64              `json:"capUsd,omitempty"`
	Purpose   string                `json:"purpose"`
	Prices    map[string]repo.Price `json:"prices"`
}

type evaluationEstimate struct {
	Cases           int     `json:"cases"`
	Runs            int     `json:"runs"`
	BaselineTokens  int64   `json:"baselineTokens"`
	CandidateTokens int64   `json:"candidateTokens"`
	BaselineUsd     float64 `json:"baselineUsd"`
	CandidateUsd    float64 `json:"candidateUsd"`
	TotalUsd        float64 `json:"totalUsd"`
	SandboxMinutes  float64 `json:"sandboxMinutes"`
	MinMinutes      float64 `json:"minMinutes"`
	MaxMinutes      float64 `json:"maxMinutes"`
	PerRoundUsd     float64 `json:"perRoundUsd"`
}

// estimateView is the answer of POST /api/v1/evaluations/estimate.
type estimateView struct {
	Estimate          evaluationEstimate `json:"estimate"`
	CapUsd            float64            `json:"capUsd"`
	NeedsConfirmation bool               `json:"needsConfirmation"`
	MonthSpentUsd     float64            `json:"monthSpentUsd"`
	MonthlyUsd        float64            `json:"monthlyUsd"`
	ConfirmAboveUsd   float64            `json:"confirmAboveUsd"`
	Change            string             `json:"change"`
	MutableModel      bool               `json:"mutableModel"`
	Cases             int                `json:"cases"`
}

type evaluationCreated struct {
	ID       string       `json:"id"`
	Estimate estimateView `json:"estimate"`
}

type verdictView struct {
	Verdict              string   `json:"verdict"`
	Delta                float64  `json:"delta"`
	Lower                float64  `json:"lower"`
	Upper                float64  `json:"upper"`
	Level                float64  `json:"level"`
	Cases                int      `json:"cases"`
	Runs                 int      `json:"runs"`
	CostRatio            *float64 `json:"costRatio"`
	CostLower            *float64 `json:"costLower"`
	CostUpper            *float64 `json:"costUpper"`
	DurationRatio        *float64 `json:"durationRatio"`
	DurationLower        *float64 `json:"durationLower"`
	DurationUpper        *float64 `json:"durationUpper"`
	EquivalentAndCheaper bool     `json:"equivalentAndCheaper"`
	BaselineRate         float64  `json:"baselineRate"`
	CandidateRate        float64  `json:"candidateRate"`
	Reason               *string  `json:"reason"`
}

type evaluationRow struct {
	ID            string             `json:"id"`
	Workspace     string             `json:"workspace"`
	Split         string             `json:"split"`
	Purpose       string             `json:"purpose"`
	Status        string             `json:"status"`
	Change        string             `json:"change"`
	Baseline      harnessSpec        `json:"baseline"`
	Candidate     harnessSpec        `json:"candidate"`
	Repeats       int                `json:"repeats"`
	Delta         float64            `json:"delta"`
	CapUsd        float64            `json:"capUsd"`
	Estimate      evaluationEstimate `json:"estimate"`
	MutableModel  bool               `json:"mutableModel"`
	Cases         int                `json:"cases"`
	SpentUsd      float64            `json:"spentUsd"`
	RunsCompleted int                `json:"runsCompleted"`
	RunsFailed    int                `json:"runsFailed"`
	Verdict       *verdictView       `json:"verdict"`
	Reason        *string            `json:"reason"`
}

type evaluationCheckpoint struct {
	Round   int     `json:"round"`
	Level   float64 `json:"level"`
	Cases   int     `json:"cases"`
	Delta   float64 `json:"delta"`
	Lower   float64 `json:"lower"`
	Upper   float64 `json:"upper"`
	Verdict string  `json:"verdict"`
}

type evaluationDetail struct {
	Evaluation  evaluationRow          `json:"evaluation"`
	Checkpoints []evaluationCheckpoint `json:"checkpoints"`
}

// minimumVerdictCases is the server's EvaluationDecider.MinimumCases: no verdict below it.
const minimumVerdictCases = 10

var compareAgents = []string{"claude-code", "codex", "cursor-cli", "command"}

var comparePurposes = []string{"compare", "harness_vs_none", "harness_ci", "gate"}

func newCompareCommand() *cobra.Command {
	var candidate, baseline, purpose string
	var repeats, cases int
	var delta, capUSD float64
	var yes bool
	cmd := &cobra.Command{
		Use:   "compare --candidate <change>",
		Short: "Compare the baseline harness with a candidate that changes one thing (member)",
		Long: "Run an evaluation: both sides on the same approved dev cases, in random interleaved order, with a verdict of\n" +
			"better, worse, equivalent or inconclusive and its interval. The candidate changes exactly one thing:\n" +
			"  model=<id>, agent=<name>[@version], harness=<git ref|none> or effort=<level>.\n" +
			"The baseline is evaluation.baseline of casebox.yml, with the harness at the repository's default branch;\n" +
			"--baseline agent=…,agent_version=…,model=…,effort=…,harness=…,max_turns=…,timeout_minutes=…,token_cap=… overrides it.\n" +
			"The prices of casebox.yml price the estimate. compare prints the estimate and asks before it starts; with --yes it\n" +
			"starts without asking when the estimate needs no confirmation. It then follows the checkpoints every 10 seconds;\n" +
			"Ctrl-C asks whether to cancel the evaluation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			root, cfg, client, err := envContext(ctx)
			if err != nil {
				return err
			}
			base, err := compareBaseline(cfg.Evaluation.Baseline, baseline, func() string { return defaultBranch(ctx, root) })
			if err != nil {
				return err
			}
			cand, defaultPurpose, err := applyChange(base, candidate)
			if err != nil {
				return err
			}
			prices, err := cfg.PriceTable()
			if err != nil {
				return err
			}
			if purpose == "" {
				purpose = defaultPurpose
			}
			req, err := compareRequest(cfg.Workspace, base, cand, prices, purpose, repeats, cases, delta, capUSD, cmd.Flags().Changed("cap"))
			if err != nil {
				return err
			}
			interrupts := make(chan os.Signal, 1)
			c := comparer{client: client, in: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout(), interval: 10 * time.Second, interrupts: interrupts,
				listen: func() func() {
					signal.Notify(interrupts, os.Interrupt)
					return func() { signal.Stop(interrupts) }
				}}
			return c.run(ctx, req, yes)
		},
	}
	cmd.Flags().StringVar(&candidate, "candidate", "", "the one change: model=<id>, agent=<name>[@version], harness=<ref|none> or effort=<level>")
	cmd.Flags().StringVar(&baseline, "baseline", "", "override evaluation.baseline of casebox.yml: agent=…,agent_version=…,model=…,effort=…,harness=…")
	cmd.Flags().IntVar(&repeats, "repeats", 3, "runs of each case on each side")
	cmd.Flags().IntVar(&cases, "cases", 0, "use at most this many approved dev cases (0: all)")
	cmd.Flags().Float64Var(&delta, "delta", 0.05, "the equivalence margin δ, as a share (0.05 is 5 points)")
	cmd.Flags().Float64Var(&capUSD, "cap", 0, "the most this evaluation may spend in USD (the organisation's cap per evaluation is the upper bound)")
	cmd.Flags().BoolVar(&yes, "yes", false, "start without asking when the estimate needs no confirmation")
	cmd.Flags().StringVar(&purpose, "purpose", "", "compare, harness_vs_none, harness_ci or gate (default compare; harness_vs_none for harness=none)")
	_ = cmd.MarkFlagRequired("candidate")
	return cmd
}

// compareBaseline starts from evaluation.baseline of casebox.yml and applies the --baseline
// overrides. The harness is the default branch unless the overrides name one.
func compareBaseline(cfg *repo.Baseline, overrides string, branch func() string) (harnessSpec, error) {
	var s harnessSpec
	if cfg != nil {
		s = harnessSpec{Agent: cfg.Agent, AgentVersion: cfg.AgentVersion, Model: cfg.Model, Effort: optional(cfg.Effort),
			Settings: agentSettings{MaxTurns: cfg.MaxTurns, TimeoutMinutes: cfg.TimeoutMinutes, TokenCap: cfg.TokenCap}}
		if cfg.Command != nil {
			s.Command = &commandTemplate{Template: cfg.Command.Template, LogGlob: optional(cfg.Command.LogGlob), LogFormat: optional(cfg.Command.LogFormat)}
		}
	}
	if strings.TrimSpace(overrides) != "" {
		for _, part := range strings.Split(overrides, ",") {
			key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if !ok || value == "" {
				return s, fmt.Errorf("--baseline %q: write key=value pairs separated by commas", part)
			}
			switch key {
			case "agent":
				name, version, hasVersion := strings.Cut(value, "@")
				s.Agent = name
				if hasVersion {
					s.AgentVersion = version
				}
			case "agent_version", "version":
				s.AgentVersion = value
			case "model":
				s.Model = value
			case "effort":
				s.Effort = &value
			case "harness":
				s.Harness = value
			case "max_turns", "timeout_minutes":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 {
					return s, fmt.Errorf("--baseline %s=%s: use a whole number above 0", key, value)
				}
				if key == "max_turns" {
					s.Settings.MaxTurns = &n
				} else {
					s.Settings.TimeoutMinutes = &n
				}
			case "token_cap":
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n < 1 {
					return s, fmt.Errorf("--baseline token_cap=%s: use a whole number above 0", value)
				}
				s.Settings.TokenCap = &n
			default:
				return s, fmt.Errorf("--baseline %s: use agent, agent_version, model, effort, harness, max_turns, timeout_minutes or token_cap", key)
			}
		}
	}
	if s.Agent == "" || s.Model == "" {
		return s, errors.New("the baseline names no agent or model; add evaluation.baseline to casebox.yml or pass --baseline agent=…,agent_version=…,model=…")
	}
	if !contains(compareAgents, s.Agent) {
		return s, fmt.Errorf("the baseline agent %q is not claude-code, codex, cursor-cli or command", s.Agent)
	}
	if s.Agent == "command" {
		if s.Command == nil || s.Command.Template == "" {
			return s, errors.New("the command agent needs evaluation.baseline.command.template in casebox.yml")
		}
	} else {
		if s.AgentVersion == "" {
			return s, fmt.Errorf("the baseline names no version of %s; versions are pinned (agent_version in casebox.yml, or agent=%s@<version>)", s.Agent, s.Agent)
		}
		s.Command = nil
	}
	if s.Harness == "" {
		s.Harness = branch()
	}
	return s, nil
}

// applyChange builds the candidate from the baseline and exactly one change. It returns the
// purpose the change implies: harness_vs_none for harness=none, compare otherwise.
func applyChange(base harnessSpec, change string) (harnessSpec, string, error) {
	const usage = "use one of model=<id>, agent=<name>[@version], harness=<ref|none> or effort=<level>"
	change = strings.TrimSpace(change)
	if strings.Contains(change, ",") {
		return base, "", fmt.Errorf("--candidate %q changes more than one thing; an evaluation changes exactly one: %s", change, usage)
	}
	key, value, ok := strings.Cut(change, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if !ok || value == "" {
		return base, "", fmt.Errorf("--candidate %q: %s", change, usage)
	}
	c := base
	purpose := "compare"
	same := func() (harnessSpec, string, error) {
		return base, "", fmt.Errorf("--candidate %s is what the baseline already uses; an evaluation changes exactly one thing", change)
	}
	switch key {
	case "model":
		if value == base.Model {
			return same()
		}
		c.Model = value
	case "effort":
		if base.Effort != nil && value == *base.Effort {
			return same()
		}
		c.Effort = &value
	case "harness":
		if value == base.Harness {
			return same()
		}
		c.Harness = value
		if value == "none" {
			purpose = "harness_vs_none"
		}
	case "agent":
		name, version, _ := strings.Cut(value, "@")
		if !contains(compareAgents, name) {
			return base, "", fmt.Errorf("--candidate agent=%s: the agent is claude-code, codex, cursor-cli or command", name)
		}
		if name == "command" || base.Agent == "command" {
			return base, "", errors.New("--candidate cannot switch to or from the command agent: its template would change too, and an evaluation changes exactly one thing")
		}
		switch {
		case name == base.Agent && version == "":
			return base, "", fmt.Errorf("--candidate agent=%s names the baseline's agent; add @<version> to change its version", name)
		case name == base.Agent && version == base.AgentVersion:
			return same()
		case name != base.Agent && version == "":
			return base, "", fmt.Errorf("--candidate agent=%s needs a pinned version: agent=%s@<version>", name, name)
		}
		c.Agent, c.AgentVersion = name, version
	default:
		return base, "", fmt.Errorf("--candidate %s: %s", key, usage)
	}
	return c, purpose, nil
}

func compareRequest(workspace string, base, cand harnessSpec, prices map[string]repo.Price, purpose string, repeats, cases int, delta, capUSD float64, capSet bool) (evaluationRequest, error) {
	req := evaluationRequest{Workspace: workspace, Baseline: base, Candidate: cand, Repeats: repeats, Delta: delta, Purpose: purpose, Prices: prices}
	if !contains(comparePurposes, purpose) {
		return req, fmt.Errorf("--purpose %s: use compare, harness_vs_none, harness_ci or gate", purpose)
	}
	if repeats < 1 {
		return req, errors.New("--repeats is at least 1")
	}
	if delta <= 0 || delta >= 1 {
		return req, errors.New("--delta is a share between 0 and 1, such as 0.05 for 5 points")
	}
	if cases < 0 {
		return req, errors.New("--cases is 0 (all) or more")
	}
	if cases > 0 {
		req.MaxCases = &cases
	}
	if capSet {
		if capUSD <= 0 {
			return req, errors.New("--cap is an amount in USD above 0")
		}
		req.CapUsd = &capUSD
	}
	for _, m := range []string{base.Model, cand.Model} {
		if _, ok := prices[m]; !ok {
			return req, fmt.Errorf("casebox.yml has no price for %s; add it under prices (USD per million tokens: input, output, and optional cache_read and cache_write)", m)
		}
	}
	if req.Prices == nil {
		req.Prices = map[string]repo.Price{}
	}
	return req, nil
}

// defaultBranch is the branch origin/HEAD points to, else init.defaultBranch, else main.
func defaultBranch(ctx context.Context, root string) string {
	if out, err := exec.CommandContext(ctx, "git", "-C", root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD").Output(); err == nil {
		if b := strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"); b != "" {
			return b
		}
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", root, "config", "init.defaultBranch").Output(); err == nil {
		if b := strings.TrimSpace(string(out)); b != "" {
			return b
		}
	}
	return "main"
}

// comparer runs the estimate, the questions, the request and the follow loop.
type comparer struct {
	client     *api.Client
	in         *bufio.Reader
	out        io.Writer
	interval   time.Duration
	interrupts <-chan os.Signal
	// listen starts the delivery of Ctrl-C to interrupts and returns the function that stops it.
	listen func() func()
}

func (c comparer) run(ctx context.Context, req evaluationRequest, yes bool) error {
	var est estimateView
	if err := c.client.Do(ctx, http.MethodPost, "/api/v1/evaluations/estimate", req, &est); err != nil {
		return fmt.Errorf("estimate the evaluation: %w", err)
	}
	printEstimate(c.out, req, est)
	if est.MonthSpentUsd+est.Estimate.TotalUsd > est.MonthlyUsd {
		return fmt.Errorf("this month's spend of %s plus the estimate of %s is over the monthly limit of %s; an Admin raises the limit, or use fewer --cases or --repeats",
			usd(est.MonthSpentUsd), usd(est.Estimate.TotalUsd), usd(est.MonthlyUsd))
	}
	if !yes || est.NeedsConfirmation {
		if !c.confirm("Start the evaluation? [y/N] ") {
			fmt.Fprintln(c.out, "Not started.")
			return nil
		}
	}
	var created evaluationCreated
	if err := c.client.Do(ctx, http.MethodPost, "/api/v1/evaluations", req, &created); err != nil {
		return fmt.Errorf("request the evaluation: %w", err)
	}
	fmt.Fprintf(c.out, "Evaluation %s is requested. The UI shows it at %s.\n", created.ID, c.page(created.ID))
	if created.Estimate.NeedsConfirmation {
		prompt := fmt.Sprintf("The estimate of %s is above the confirmation threshold of %s. Confirm the spend? [y/N] ",
			usd(created.Estimate.Estimate.TotalUsd), usd(created.Estimate.ConfirmAboveUsd))
		if !c.confirm(prompt) {
			fmt.Fprintf(c.out, "Evaluation %s waits for confirmation. Confirm or cancel it at %s.\n", created.ID, c.page(created.ID))
			return nil
		}
		if err := c.client.Do(ctx, http.MethodPost, "/api/v1/evaluations/"+url.PathEscape(created.ID)+"/confirmation", nil, nil); err != nil {
			return fmt.Errorf("confirm the evaluation: %w", err)
		}
		fmt.Fprintln(c.out, "Confirmed.")
	}
	return c.follow(ctx, created.ID)
}

func (c comparer) page(id string) string {
	return c.client.Server + "/evaluations/" + url.PathEscape(id)
}

// confirm asks a yes or no question; anything but y or yes, or the end of the input, is no.
func (c comparer) confirm(prompt string) bool {
	fmt.Fprint(c.out, prompt)
	line, err := c.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(c.out)
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// follow reads the evaluation every interval and prints each change: the status, the runs and
// spend, each new checkpoint, and at the end the verdict or the cancellation.
func (c comparer) follow(ctx context.Context, id string) error {
	if c.listen != nil {
		defer c.listen()()
	}
	fmt.Fprintln(c.out, "Following the evaluation. Ctrl-C asks whether to cancel it.")
	printed := map[int]bool{}
	var lastStatus, lastProgress string
	failures := 0
	for {
		var d evaluationDetail
		err := c.client.Do(ctx, http.MethodGet, "/api/v1/evaluations/"+url.PathEscape(id), nil, &d)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil && api.StatusOf(err) >= 400 && api.StatusOf(err) < 500:
			return fmt.Errorf("read the evaluation: %w", err)
		case err != nil:
			failures++
			if failures >= 6 {
				return fmt.Errorf("read the evaluation: %w; the evaluation goes on at %s", err, c.page(id))
			}
			fmt.Fprintf(c.out, "Cannot read the evaluation (%v); trying again.\n", err)
		default:
			failures = 0
			e := d.Evaluation
			if e.Status != lastStatus {
				lastStatus = e.Status
				switch e.Status {
				case "awaiting_confirmation":
					fmt.Fprintf(c.out, "It waits for confirmation at %s.\n", c.page(id))
				case "running":
					fmt.Fprintln(c.out, "Running.")
				}
			}
			progress := fmt.Sprintf("Runs: %d completed, %d failed, of %d planned. Spent %s of the %s cap.",
				e.RunsCompleted, e.RunsFailed, e.Estimate.Runs, usd(e.SpentUsd), usd(e.CapUsd))
			if progress != lastProgress && (e.RunsCompleted+e.RunsFailed > 0 || lastProgress != "") {
				fmt.Fprintln(c.out, progress)
			}
			lastProgress = progress
			for _, cp := range d.Checkpoints {
				if !printed[cp.Round] {
					printed[cp.Round] = true
					fmt.Fprintln(c.out, checkpointLine(cp))
				}
			}
			switch e.Status {
			case "done":
				printVerdict(c.out, e)
				return nil
			case "cancelled":
				fmt.Fprintf(c.out, "Cancelled: %s. Spent %s of the %s estimate on %d completed runs.\n",
					deref(e.Reason, "no reason given"), usd(e.SpentUsd), usd(e.Estimate.TotalUsd), e.RunsCompleted)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.interrupts:
			fmt.Fprintln(c.out)
			if !c.confirm(fmt.Sprintf("Cancel evaluation %s? No leaves it running. [y/N] ", id)) {
				fmt.Fprintf(c.out, "Evaluation %s goes on. Follow it at %s.\n", id, c.page(id))
				return nil
			}
			if err := c.client.Do(ctx, http.MethodPost, "/api/v1/evaluations/"+url.PathEscape(id)+"/cancellation",
				map[string]string{"reason": "cancelled from casebox compare"}, nil); err != nil {
				return fmt.Errorf("cancel the evaluation: %w", err)
			}
			fmt.Fprintf(c.out, "Evaluation %s is cancelled. No new run starts; the cost of runs already recorded stays.\n", id)
			return nil
		case <-time.After(c.interval):
		}
	}
}

func printEstimate(out io.Writer, req evaluationRequest, est estimateView) {
	e := est.Estimate
	fmt.Fprintf(out, "Estimate for workspace %s\n", req.Workspace)
	fmt.Fprintf(out, "  Baseline:     %s\n", sideText(req.Baseline))
	fmt.Fprintf(out, "  Candidate:    %s\n", sideText(req.Candidate))
	fmt.Fprintf(out, "  Change:       %s\n", changeText(req.Baseline, req.Candidate, est.Change))
	fmt.Fprintf(out, "  Cases:        %d approved dev cases × 2 sides × %d repeats = %d runs\n", e.Cases, req.Repeats, e.Runs)
	fmt.Fprintf(out, "  Baseline:     %s tokens, %s\n", thousands(e.BaselineTokens), usd(e.BaselineUsd))
	fmt.Fprintf(out, "  Candidate:    %s tokens, %s\n", thousands(e.CandidateTokens), usd(e.CandidateUsd))
	fmt.Fprintf(out, "  Total:        %s (%s a round)\n", usd(e.TotalUsd), usd(e.PerRoundUsd))
	fmt.Fprintf(out, "  Sandbox time: %s\n", minutesText(e.SandboxMinutes))
	fmt.Fprintf(out, "  Duration:     %s to %s, by how many sandboxes the workers run at once\n", minutesText(e.MinMinutes), minutesText(e.MaxMinutes))
	fmt.Fprintf(out, "  Cap:          %s for this evaluation\n", usd(est.CapUsd))
	fmt.Fprintf(out, "  This month:   %s spent of the %s monthly limit\n", usd(est.MonthSpentUsd), usd(est.MonthlyUsd))
	if est.NeedsConfirmation {
		fmt.Fprintf(out, "  Confirmation: needed, because the total is above %s\n", usd(est.ConfirmAboveUsd))
	} else {
		fmt.Fprintf(out, "  Confirmation: not needed; the threshold is %s\n", usd(est.ConfirmAboveUsd))
	}
	fmt.Fprintln(out, "Estimates use each case's past runs, else its source session, else 400,000 tokens and 12 minutes a run.")
	if e.TotalUsd > est.CapUsd {
		fmt.Fprintf(out, "Warning: the estimate is above the cap. The evaluation stops before a round that would pass %s, and the verdict is then inconclusive.\n", usd(est.CapUsd))
	}
	if est.MutableModel {
		fmt.Fprintf(out, "Warning: a model ID (%s) names no fixed version. The provider can change the model behind it, so the result may not repeat.\n",
			modelsText(req.Baseline.Model, req.Candidate.Model))
	}
}

func checkpointLine(cp evaluationCheckpoint) string {
	return fmt.Sprintf("Round %d: Δ %s, %s interval %s, %d cases: %s",
		cp.Round, points(cp.Delta), levelText(cp.Level), intervalPoints(cp.Lower, cp.Upper), cp.Cases, cp.Verdict)
}

func printVerdict(out io.Writer, e evaluationRow) {
	v := e.Verdict
	if v == nil {
		fmt.Fprintln(out, "The evaluation is done, but the server shows no verdict.")
		return
	}
	fmt.Fprintf(out, "Verdict: %s\n", v.Verdict)
	fmt.Fprintf(out, "  Δ pass rate (candidate − baseline): %s, %s interval %s\n", points(v.Delta), levelText(v.Level), intervalPoints(v.Lower, v.Upper))
	fmt.Fprintf(out, "  Rule: %s\n", verdictRule(*v, e.Delta))
	fmt.Fprintf(out, "  Pass rate: baseline %.1f%%, candidate %.1f%%, over %d cases and %d runs\n", v.BaselineRate*100, v.CandidateRate*100, v.Cases, v.Runs)
	if v.CostRatio != nil {
		fmt.Fprintf(out, "  Cost per task, candidate ÷ baseline: %s, %s interval %s\n", ratio(*v.CostRatio), levelText(v.Level), ratioInterval(v.CostLower, v.CostUpper))
	}
	if v.DurationRatio != nil {
		fmt.Fprintf(out, "  Duration, candidate ÷ baseline: %s, %s interval %s\n", ratio(*v.DurationRatio), levelText(v.Level), ratioInterval(v.DurationLower, v.DurationUpper))
	}
	if v.EquivalentAndCheaper {
		fmt.Fprintln(out, "  Equivalent and cheaper: the same quality within ±δ, and the cost interval lies entirely below 1.")
	}
	fmt.Fprintf(out, "  Spent %s against an estimate of %s; %d runs failed to run.\n", usd(e.SpentUsd), usd(e.Estimate.TotalUsd), e.RunsFailed)
	if e.MutableModel {
		fmt.Fprintf(out, "  Warning: a model ID (%s) names no fixed version, so the result may not repeat.\n", modelsText(e.Baseline.Model, e.Candidate.Model))
	}
	fmt.Fprintln(out, "A verdict is a controlled comparison: both sides ran the same cases in random interleaved order. Steering metrics only observe work as it happened.")
}

// verdictRule names the rule the verdict met, or why it is inconclusive.
func verdictRule(v verdictView, delta float64) string {
	level := levelText(v.Level)
	switch v.Verdict {
	case "better":
		return fmt.Sprintf("the %s interval lies entirely above 0", level)
	case "worse":
		return fmt.Sprintf("the %s interval lies entirely below 0", level)
	case "equivalent":
		return fmt.Sprintf("the %s interval lies within ±%s (δ)", level, strings.TrimPrefix(points(delta), "+"))
	}
	switch {
	case v.Reason != nil && *v.Reason == "budget":
		return "inconclusive, because the budget ran out: the next round would have passed the cap"
	case v.Reason != nil && *v.Reason != "":
		return "inconclusive: " + *v.Reason
	case v.Cases < minimumVerdictCases:
		return fmt.Sprintf("inconclusive, because only %d cases have completed runs on both sides; a verdict needs at least %d", v.Cases, minimumVerdictCases)
	default:
		return fmt.Sprintf("inconclusive, because the %s interval neither lies on one side of 0 nor within ±%s", level, strings.TrimPrefix(points(delta), "+"))
	}
}

func sideText(s harnessSpec) string {
	parts := []string{s.Agent}
	if s.AgentVersion != "" {
		parts[0] += " " + s.AgentVersion
	}
	parts = append(parts, "model "+s.Model)
	if s.Effort != nil {
		parts = append(parts, "effort "+*s.Effort)
	}
	if s.Harness == "none" {
		parts = append(parts, "no harness")
	} else {
		parts = append(parts, "harness at "+s.Harness)
	}
	return strings.Join(parts, ", ")
}

func changeText(b, c harnessSpec, change string) string {
	switch change {
	case "model":
		return fmt.Sprintf("model %s → %s", b.Model, c.Model)
	case "agent", "agentVersion":
		return fmt.Sprintf("agent %s %s → %s %s", b.Agent, b.AgentVersion, c.Agent, c.AgentVersion)
	case "effort":
		return fmt.Sprintf("effort %s → %s", deref(b.Effort, "default"), deref(c.Effort, "default"))
	case "harness":
		return fmt.Sprintf("harness %s → %s", b.Harness, c.Harness)
	}
	return change
}

func modelsText(a, b string) string {
	if a == b {
		return a
	}
	return a + " or " + b
}

func usd(v float64) string { return fmt.Sprintf("%.2f USD", v) }

// points is a share as signed percentage points, such as +4.0 points.
func points(share float64) string { return fmt.Sprintf("%+.1f points", share*100) }

func intervalPoints(lo, hi float64) string {
	return fmt.Sprintf("%+.1f to %+.1f points", lo*100, hi*100)
}

func levelText(level float64) string { return fmt.Sprintf("%.0f%%", level*100) }

func ratio(r float64) string { return fmt.Sprintf("%.2f×", r) }

func ratioInterval(lo, hi *float64) string {
	if lo == nil || hi == nil {
		return "not available"
	}
	return fmt.Sprintf("%.2f to %.2f", *lo, *hi)
}

func minutesText(m float64) string {
	if m < 90 {
		return fmt.Sprintf("%.0f minutes", math.Round(m))
	}
	return fmt.Sprintf("%.1f hours", m/60)
}

func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 && s[i-1] != '-' {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
