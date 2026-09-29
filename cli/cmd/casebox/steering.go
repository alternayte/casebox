package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/steering"
)

func newSteeringCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "steering", Short: "Steering classification"}
	var limit, concurrency int
	var team bool
	check := &cobra.Command{
		Use:   "check",
		Short: "Classifier agreement on the labelled test set",
		Long: "Run the steering classifier with this machine's analysis model on the labelled set shipped in the CLI\n" +
			"(synthetic transcripts) and report how often it agrees with the labels, step by step.\n" +
			"With --team, compare the model's labels with your team's relabels on the server instead; no model is called.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if team {
				return teamAgreement(cmd.Context(), cmd.OutOrStdout())
			}
			return labelledAgreement(cmd.Context(), cmd.OutOrStdout(), limit, concurrency)
		},
	}
	check.Flags().IntVar(&limit, "limit", 0, "classify only the first n labelled interventions (default: all)")
	check.Flags().IntVar(&concurrency, "concurrency", 4, "model calls at once")
	check.Flags().BoolVar(&team, "team", false, "compare with the team's relabels on the server")
	cmd.AddCommand(check)
	return cmd
}

func labelledAgreement(ctx context.Context, out io.Writer, limit, concurrency int) error {
	cfg, err := analysis.FromEnv()
	if err != nil {
		return err
	}
	set, err := steering.LabelledSet()
	if err != nil {
		return err
	}
	if limit > 0 && limit < len(set) {
		set = set[:limit]
	}
	fmt.Fprintf(out, "Classifying %d labelled interventions with %s (prompt %s)…\n", len(set), cfg, steering.PromptVersion)
	classifier := steering.Classifier{Model: analysis.New(cfg)}
	pairs, outcomes, err := classifier.Check(ctx, set, concurrency)
	if err != nil {
		return fmt.Errorf("every call failed: %w", err)
	}
	a := steering.Agree(pairs)

	var model string
	low, invalid, failed := 0, 0, 0
	var firstInvalid, firstFailed error
	for i, o := range outcomes {
		switch {
		case o.Err == nil && !pairs[i].Classified:
			low++
		case errors.Is(o.Err, steering.ErrInvalidOutput):
			invalid++
			if firstInvalid == nil {
				firstInvalid = o.Err
			}
		case o.Err != nil:
			failed++
			if firstFailed == nil {
				firstFailed = o.Err
			}
		}
		if model == "" {
			model = o.Model
		}
	}
	if model != "" && model != cfg.Model {
		fmt.Fprintf(out, "The provider reported model %s.\n", model)
	}
	fmt.Fprintln(out, scoreLine("Intent", a.Intent.N, a.Intent.Agreement, a.Intent.Kappa))
	fmt.Fprintln(out, scoreLine("What went wrong", a.WentWrong.N, a.WentWrong.Agreement, a.WentWrong.Kappa))
	fmt.Fprintln(out, scoreLine("Prevention", a.Prevention.N, a.Prevention.Agreement, a.Prevention.Kappa))
	fmt.Fprintf(out, "Unclassified: %d of %d (%.0f%%): %d below %.1f confidence, %d invalid, %d failed.\n",
		a.Unclassified, a.Total, 100*a.UnclassifiedShare(), low, steering.Threshold, invalid, failed)
	if firstInvalid != nil {
		fmt.Fprintf(out, "First invalid answer: %v\n", firstInvalid)
	}
	if firstFailed != nil {
		fmt.Fprintf(out, "First failed call: %v\n", firstFailed)
	}

	fmt.Fprintln(out, "Intents, labelled (rows) against the model (columns):")
	fmt.Fprintf(out, "  %-14s", "")
	for _, col := range steering.Intents {
		fmt.Fprintf(out, " %13s", col)
	}
	fmt.Fprintln(out)
	for _, row := range steering.Intents {
		fmt.Fprintf(out, "  %-14s", row)
		for _, col := range steering.Intents {
			fmt.Fprintf(out, " %13d", a.Confusion[row][col])
		}
		fmt.Fprintln(out)
	}
	return nil
}

func scoreLine(step string, n int, agreement, kappa float64) string {
	if n == 0 {
		return fmt.Sprintf("%s: no pairs to compare.", step)
	}
	return fmt.Sprintf("%s: agreement %.2f, kappa %.2f (n=%d).", step, agreement, kappa, n)
}

// teamAgreement is the answer of GET /api/v1/steering/agreement (docs/specs/steering.md).
type teamScore struct {
	Agreement *float64 `json:"agreement"`
	Kappa     *float64 `json:"kappa"`
	N         int      `json:"n"`
}

func teamAgreement(ctx context.Context, out io.Writer) error {
	client, err := cliClient()
	if err != nil {
		return err
	}
	var res struct {
		Hidden     bool      `json:"hidden"`
		K          int       `json:"k"`
		People     int       `json:"people"`
		Events     int       `json:"events"`
		Intent     teamScore `json:"intent"`
		WentWrong  teamScore `json:"wentWrong"`
		Prevention teamScore `json:"prevention"`
	}
	if err := client.Do(ctx, http.MethodGet, "/api/v1/steering/agreement", nil, &res); err != nil {
		return err
	}
	if res.Hidden {
		fmt.Fprintf(out, "Hidden: fewer than %d people are behind the team's relabels.\n", res.K)
		return nil
	}
	fmt.Fprintf(out, "The team relabelled %d events; %d people are behind them.\n", res.Events, res.People)
	for _, s := range []struct {
		step  string
		score teamScore
	}{{"Intent", res.Intent}, {"What went wrong", res.WentWrong}, {"Prevention", res.Prevention}} {
		if s.score.N == 0 || s.score.Agreement == nil || s.score.Kappa == nil {
			fmt.Fprintf(out, "%s: no pairs to compare.\n", s.step)
			continue
		}
		fmt.Fprintln(out, scoreLine(s.step, s.score.N, *s.score.Agreement, *s.score.Kappa))
	}
	return nil
}

func cliClient() (*api.Client, error) {
	creds, err := config.LoadCredentials()
	if err != nil {
		return nil, err
	}
	if creds.CLIToken == "" {
		return nil, errors.New("this machine has no CLI login; run casebox init or casebox join")
	}
	return api.New(creds.Server, creds.CLIToken), nil
}

// steeringStatus is the answer of GET /api/v1/steering/status.
type steeringStatus struct {
	Sessions        int  `json:"sessions"`
	Interventions   int  `json:"interventions"`
	Pending         int  `json:"pending"`
	Unclassified    int  `json:"unclassified"`
	WorkerSeen      bool `json:"workerSeen"`
	AnalysisWorkers int  `json:"analysisWorkers"`
}

// steeringAfterImport runs detection for the repository, waits while the workers classify, and
// prints the headline of the steering report.
func steeringAfterImport(ctx context.Context, out io.Writer, repo string, days int, wait time.Duration) error {
	client, err := cliClient()
	if err != nil {
		return err
	}
	var st steeringStatus
	if err := client.Do(ctx, http.MethodPost, "/api/v1/steering/refresh", map[string]string{"repo": repo}, &st); err != nil {
		return fmt.Errorf("start steering detection: %w", err)
	}
	switch {
	case st.Pending > 0 && st.AnalysisWorkers == 0:
		fmt.Fprintf(out, "%d interventions wait for a worker with an analysis model; start one with CASEBOX_WORKER_TOKEN=<token> CASEBOX_ANALYSIS_PROVIDER=<openai, anthropic or cursor-agent> CASEBOX_ANALYSIS_MODEL=<model> casebox worker.\n", st.Pending)
	case st.Pending > 0 && wait > 0:
		fmt.Fprintf(out, "Classifying %d interventions (waiting up to %s)…\n", st.Pending, wait)
		deadline := time.Now().Add(wait)
		for st.Pending > 0 && st.AnalysisWorkers > 0 && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			if err := client.Do(ctx, http.MethodGet, "/api/v1/steering/status?repo="+url.QueryEscape(repo), nil, &st); err != nil {
				return err
			}
		}
		switch {
		case st.Pending > 0 && st.AnalysisWorkers == 0:
			fmt.Fprintf(out, "The analysis worker stopped with %d interventions left; start casebox worker with CASEBOX_ANALYSIS_PROVIDER and CASEBOX_ANALYSIS_MODEL set.\n", st.Pending)
		case st.Pending > 0:
			fmt.Fprintf(out, "%d interventions are still pending; the report fills in as the worker finishes.\n", st.Pending)
		}
	}

	var report steeringReport
	days = min(days, 365) // a report covers at most 400 days
	from := time.Now().AddDate(0, 0, -days).UTC().Format(time.DateOnly)
	path := "/api/v1/steering/report?repo=" + url.QueryEscape(repo) + "&from=" + from
	if err := client.Do(ctx, http.MethodGet, path, nil, &report); err != nil {
		return fmt.Errorf("read the steering report: %w", err)
	}
	report.print(out, repo, days)
	fmt.Fprintf(out, "Report: %s/?repo=%s\n", client.Server, url.QueryEscape(repo))
	return nil
}

type rate struct {
	Value    float64   `json:"value"`
	N        int       `json:"n"`
	Interval []float64 `json:"interval"`
}

type spread struct {
	Median float64 `json:"median"`
	P75    float64 `json:"p75"`
}

type steeringReport struct {
	K      int  `json:"k"`
	Solo   bool `json:"solo"`
	Themes []struct {
		WentWrong      string `json:"wentWrong"`
		Label          string `json:"label"`
		Corrections    int    `json:"corrections"`
		People         int    `json:"people"`
		HarnessFixable bool   `json:"harnessFixable"`
		Quotes         []struct {
			Text string `json:"text"`
		} `json:"quotes"`
	} `json:"themes"`
	Hidden struct {
		Themes int `json:"themes"`
	} `json:"hidden"`
	Coverage struct {
		Sessions      int    `json:"sessions"`
		People        int    `json:"people"`
		Interventions int    `json:"interventions"`
		Pending       int    `json:"pending"`
		Unclassified  int    `json:"unclassified"`
		PromptMode    string `json:"promptMode"`
	} `json:"coverage"`
	Headline struct {
		CorrectionFreeRate     *rate `json:"correctionFreeRate"`
		CorrectionsPerWorkItem *struct {
			InSession   *spread `json:"inSession"`
			BeforeMerge *spread `json:"beforeMerge"`
			AfterMerge  *spread `json:"afterMerge"`
			N           int     `json:"n"`
		} `json:"correctionsPerWorkItem"`
		AutonomousRun *struct {
			Turns     *spread `json:"turns"`
			ToolCalls *spread `json:"toolCalls"`
			N         int     `json:"n"`
		} `json:"autonomousRun"`
		AfterMergeRate  *rate `json:"afterMergeRate"`
		AbandonmentRate *rate `json:"abandonmentRate"`
	} `json:"headline"`
}

func (r steeringReport) print(out io.Writer, repo string, days int) {
	c := r.Coverage
	fmt.Fprintf(out, "Steering in %s, last %d days: %d sessions, %d people, %d interventions (%d unclassified, %d pending), prompt mode %s.\n",
		repo, days, c.Sessions, c.People, c.Interventions, c.Unclassified, c.Pending, c.PromptMode)
	hidden := fmt.Sprintf("none, or hidden because fewer than %d people are behind it", r.K)
	if r.Solo {
		fmt.Fprintf(out, "Only your own sessions: while you are the only person here, nothing is hidden. A second person turns on the %d-person minimum for good.\n", r.K)
		hidden = "none yet"
	}
	h := r.Headline
	fmt.Fprintf(out, "Correction-free work items: %s\n", rateText(h.CorrectionFreeRate, hidden))
	if p := h.CorrectionsPerWorkItem; p != nil {
		fmt.Fprintf(out, "Corrections per work item: in session %s, before merge %s, after merge %s (n=%d)\n",
			spreadText(p.InSession), spreadText(p.BeforeMerge), spreadText(p.AfterMerge), p.N)
	} else {
		fmt.Fprintf(out, "Corrections per work item: %s\n", hidden)
	}
	if a := h.AutonomousRun; a != nil && a.N > 0 {
		fmt.Fprintf(out, "Autonomous run before the first correction: %s turns, %s tool calls (n=%d)\n", spreadText(a.Turns), spreadText(a.ToolCalls), a.N)
	} else {
		fmt.Fprintf(out, "Autonomous run before the first correction: %s\n", hidden)
	}
	fmt.Fprintf(out, "After-merge rate: %s\n", rateText(h.AfterMergeRate, hidden))
	fmt.Fprintf(out, "Abandonment rate: %s\n", rateText(h.AbandonmentRate, hidden))
	if len(r.Themes) > 0 {
		fmt.Fprintln(out, "Top correction themes:")
	}
	for i, t := range r.Themes {
		if i == 5 {
			fmt.Fprintf(out, "  …and %d more in the web report.\n", len(r.Themes)-5)
			break
		}
		name := t.WentWrong
		if t.Label != "" {
			name += ": " + t.Label
		}
		fix := ""
		if t.HarnessFixable {
			fix = ", a harness rule can prevent it"
		}
		fmt.Fprintf(out, "  %d. %s (%d corrections, %d people%s)\n", i+1, name, t.Corrections, t.People, fix)
		if len(t.Quotes) > 0 {
			q := strings.Join(strings.Fields(t.Quotes[0].Text), " ")
			if len(q) > 160 {
				q = q[:160] + "…"
			}
			fmt.Fprintf(out, "     \"%s\"\n", q)
		}
	}
	if r.Hidden.Themes > 0 {
		fmt.Fprintf(out, "%d more themes are hidden: fewer than %d people are behind each.\n", r.Hidden.Themes, r.K)
	}
}

func rateText(r *rate, hidden string) string {
	if r == nil {
		return hidden
	}
	if len(r.Interval) == 2 {
		return fmt.Sprintf("%.0f%% (n=%d, 95%% interval %.0f–%.0f%%)", 100*r.Value, r.N, 100*r.Interval[0], 100*r.Interval[1])
	}
	return fmt.Sprintf("%.0f%% (n=%d)", 100*r.Value, r.N)
}

func spreadText(s *spread) string {
	if s == nil {
		return "none"
	}
	return fmt.Sprintf("median %g, p75 %g", s.Median, s.P75)
}
