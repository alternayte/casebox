package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/buildinfo"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/worker"
)

type proposerAnswer struct {
	CIRun   *string `json:"ciRun"`
	Started []struct {
		Pattern  string `json:"pattern"`
		Proposal string `json:"proposal"`
	} `json:"started"`
	Skipped []struct {
		Pattern *string `json:"pattern"`
		Reason  string  `json:"reason"`
	} `json:"skipped"`
	Diet bool `json:"diet"`
}

type proposerProposal struct {
	ID      string  `json:"id"`
	Pattern *string `json:"pattern"`
	Kind    string  `json:"kind"`
	Status  string  `json:"status"`
	Reason  *string `json:"reason"`
	PrURL   *string `json:"prUrl"`
}

func newProposeCommand() *cobra.Command {
	var pattern, workerMode string
	var scheduled bool
	var budgetRuns int
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "propose (--pattern <id> | --scheduled)",
		Short: "Run the proposer: harness edits for a pattern, tested on held-out cases before a pull request",
		Long: "Run the proposer (docs/specs/self-evolution.md). For each open pattern (or the one --pattern names) it drafts 3 to 5\n" +
			"small harness edits with the analysis model, scores them on a dev batch against the cached baseline, and sends the\n" +
			"best to the held-out gate. A passing gate opens a pull request; a person merges it.\n" +
			"--scheduled is the Action's weekly run: every open pattern of the workspace, and the harness diet once a quarter.\n" +
			"The baseline, prices and repeats come from casebox.yml. It uses CASEBOX_SERVER and CASEBOX_TOKEN (a ci token) when\n" +
			"set, else this machine's login (a Member). The search budget is --budget-runs agent runs per pattern, and the\n" +
			"proposer spends at most its share of the monthly budget. --worker inline runs the jobs here, and waits for them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (pattern == "") == !scheduled {
				return errors.New("name one pattern with --pattern <id>, or run every open pattern with --scheduled")
			}
			if workerMode != "none" && workerMode != "inline" {
				return fmt.Errorf("--worker %s: use none or inline", workerMode)
			}
			ctx := cmd.Context()
			root, cfg, err := repoConfig(ctx)
			if err != nil {
				return err
			}
			if cfg.Workspace == "" {
				return cbx.Errorf(cbx.NoWorkspace, "casebox.yml names no workspace; the proposer runs in a workspace repository")
			}
			client, err := proposerClient(cfg)
			if err != nil {
				return err
			}
			spec, err := compareBaseline(cfg.Evaluation.Baseline, cfg.Harness.Shared, "", func() string { return "HEAD" })
			if err != nil {
				return err
			}
			prices, err := cfg.PriceTable()
			if err != nil {
				return err
			}
			body := map[string]any{
				"workspace": cfg.Workspace, "spec": spec, "prices": prices, "repeats": cfg.FullRepeats(), "budgetRuns": budgetRuns,
				"repo": repo.Current(ctx, root).Repo,
			}
			if pattern != "" {
				body["pattern"] = pattern
			}
			var answer proposerAnswer
			if err := client.Do(ctx, http.MethodPost, "/api/v1/proposer/runs", body, &answer); err != nil {
				return fmt.Errorf("start the proposer: %w", err)
			}
			out := cmd.OutOrStdout()
			for _, s := range answer.Started {
				fmt.Fprintf(out, "Pattern %s: proposal %s is searching.\n", s.Pattern, s.Proposal)
			}
			for _, s := range answer.Skipped {
				fmt.Fprintf(out, "Pattern %s: skipped, %s.\n", deref(s.Pattern, "(all)"), s.Reason)
			}
			if answer.Diet {
				fmt.Fprintln(out, "The harness diet is running: each large section and skill is tried removed on the dev batch.")
			}
			if answer.CIRun == nil || (len(answer.Started) == 0 && !answer.Diet) {
				return nil
			}
			if workerMode != "inline" {
				fmt.Fprintln(out, "The organisation's workers run the search and the gate; pull requests open when a gate passes.")
				return nil
			}
			followCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return followProposer(followCtx, client, *answer.CIRun, out, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&pattern, "pattern", "", "the pattern to propose for now")
	cmd.Flags().BoolVar(&scheduled, "scheduled", false, "every open pattern of the workspace, and the diet once a quarter (the Action's weekly run)")
	cmd.Flags().StringVar(&workerMode, "worker", "none", "inline: run the proposer's jobs on this machine and wait for them")
	cmd.Flags().IntVar(&budgetRuns, "budget-runs", 40, "the search budget per pattern, in agent runs")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Hour, "how long an inline worker runs")
	return cmd
}

// proposerClient is CASEBOX_TOKEN's client (a ci token in the Action) when set, else this
// machine's login.
func proposerClient(cfg repo.Config) (*api.Client, error) {
	if token := os.Getenv("CASEBOX_TOKEN"); token != "" {
		server := os.Getenv("CASEBOX_SERVER")
		if server == "" {
			server = cfg.Server
		}
		if server == "" {
			return nil, errors.New("set CASEBOX_SERVER, or server in casebox.yml")
		}
		return api.New(server, token), nil
	}
	return cliClient()
}

// followProposer runs the proposer run's jobs here until none waits and no proposal searches or
// gates, then prints each proposal's outcome.
func followProposer(ctx context.Context, client *api.Client, run string, out, log io.Writer) error {
	handlers, err := workerHandlers(ctx, client, log)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	w := &worker.Worker{Client: client, ID: fmt.Sprintf("propose-%s-%d", host, os.Getpid()), Version: buildinfo.Version,
		Log: log, Handlers: handlers, Scope: run, MaxIdle: 5 * time.Second}
	workerCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = w.Run(workerCtx) }()
	for {
		var v struct {
			Status    string             `json:"status"`
			SpentUsd  float64            `json:"spentUsd"`
			Proposals []proposerProposal `json:"proposals"`
		}
		err := client.Do(ctx, http.MethodGet, "/api/v1/ci/runs/"+url.PathEscape(run), nil, &v)
		if err == nil && v.Status == "done" {
			for _, p := range v.Proposals {
				line := fmt.Sprintf("Proposal %s (%s): %s", p.ID, p.Kind, p.Status)
				if p.PrURL != nil {
					line += ", " + *p.PrURL
				}
				if p.Reason != nil {
					line += ". " + *p.Reason
				}
				fmt.Fprintln(out, line)
			}
			fmt.Fprintf(out, "Spent %s on searches and gates.\n", usd(v.SpentUsd))
			return nil
		}
		select {
		case <-ctx.Done():
			return exitError{2, errors.New("the proposer run did not finish before --timeout; its jobs wait for a worker")}
		case <-time.After(10 * time.Second):
		}
	}
}
