package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/buildinfo"
	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/gitmirror"
	"github.com/alternayte/casebox/cli/internal/propose"
	"github.com/alternayte/casebox/cli/internal/worker"
)

func newWorkerCommand() *cobra.Command {
	var server, id string
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run a worker",
		Long: "Run a worker: it leases jobs from the server and runs them on this host.\n" +
			"It reads its worker token from CASEBOX_WORKER_TOKEN, and the tokens for cloning from GITHUB_TOKEN and, for Azure DevOps\n" +
			"Server, AZURE_DEVOPS_TOKEN (a personal access token with Code (Read)).\n" +
			"Model API keys stay in this host's environment; the server never sees them.\n" +
			"Steering classification, pattern splits and proposal drafts need an analysis model: CASEBOX_ANALYSIS_PROVIDER (anthropic, openai for\n" +
			"any OpenAI-compatible API, or cursor-agent), CASEBOX_ANALYSIS_MODEL, and optionally CASEBOX_ANALYSIS_BASE_URL and\n" +
			"CASEBOX_ANALYSIS_API_KEY (default ANTHROPIC_API_KEY or OPENAI_API_KEY). cursor-agent uses this machine's Cursor CLI\n" +
			"login and needs no key; its model defaults to auto.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token := os.Getenv("CASEBOX_WORKER_TOKEN")
			if token == "" {
				return errors.New("set CASEBOX_WORKER_TOKEN to a worker token (Settings → Tokens, or POST /api/v1/tokens)")
			}
			if server == "" {
				server = os.Getenv("CASEBOX_SERVER")
			}
			if server == "" {
				if creds, err := config.LoadCredentials(); err == nil {
					server = creds.Server
				}
			}
			if server == "" {
				return errors.New("name the server with --server or CASEBOX_SERVER")
			}
			if id == "" {
				host, _ := os.Hostname()
				id = fmt.Sprintf("%s-%d", host, os.Getpid())
			}
			client := api.New(server, token)
			handlers, err := workerHandlers(client, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			w := &worker.Worker{
				Client:   client,
				ID:       id,
				Version:  buildinfo.Version,
				Log:      cmd.ErrOrStderr(),
				Handlers: handlers,
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			shutdown, err := worker.Telemetry(ctx, buildinfo.Version)
			if err != nil {
				return fmt.Errorf("start OpenTelemetry: %w", err)
			}
			defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
			fmt.Fprintf(cmd.OutOrStdout(), "Worker %s is running against %s. Ctrl-C stops it.\n", id, server)
			if err := w.Run(ctx); err != nil && !errors.Is(err, ctx.Err()) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "the Casebox server (default: CASEBOX_SERVER, then this machine's credentials)")
	cmd.Flags().StringVar(&id, "id", "", "the worker ID (default: host name and process ID)")
	return cmd
}

// workerHandlers are the job kinds this host can run, and a line on each capability it lacks.
func workerHandlers(client *api.Client, out io.Writer) (map[string]worker.Handler, error) {
	mirrors, err := config.Path("worker", "mirrors")
	if err != nil {
		return nil, err
	}
	remotes := &gitmirror.Remotes{Client: client, GitHubToken: os.Getenv("GITHUB_TOKEN"), AzureDevOpsToken: os.Getenv("AZURE_DEVOPS_TOKEN")}
	jobs := worker.RepoJobs{Client: client, MirrorRoot: mirrors, Remotes: remotes}
	steer := worker.Steering{Client: client, MirrorRoot: mirrors, Remotes: remotes, Concurrency: 4}
	handlers := map[string]worker.Handler{
		"entire.fetch": jobs.Entire,
		"gitai.fetch":  jobs.GitAI,
		"steering.pr":  steer.PR,
	}
	// Only a worker with an analysis model leases classification, pattern and draft jobs.
	model, err := analysis.FromEnv()
	switch {
	case err == nil:
		steer.Model = analysis.New(model)
		drafts := propose.Jobs{Client: client, MirrorRoot: mirrors, Remotes: remotes, Model: steer.Model}
		handlers["steering.classify"] = steer.Classify
		handlers["pattern.cluster"] = steer.Cluster
		handlers["proposal.draft"] = drafts.Draft
		fmt.Fprintf(out, "Analysis model: %s at %s; this worker classifies steering and drafts proposals.\n", model, model.BaseURL)
	case errors.Is(err, analysis.ErrNotConfigured):
		fmt.Fprintln(out, "Analysis model: not configured, so this worker does not classify steering or draft proposals. Set CASEBOX_ANALYSIS_PROVIDER and CASEBOX_ANALYSIS_MODEL to enable it.")
	default:
		fmt.Fprintf(out, "Analysis model: %v. This worker does not classify steering or draft proposals.\n", err)
	}
	return handlers, nil
}
