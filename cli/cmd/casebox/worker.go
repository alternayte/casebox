package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/buildinfo"
	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/sandbox/providers"
	"github.com/alternayte/casebox/cli/internal/worker"
)

func newWorkerCommand() *cobra.Command {
	var server, id string
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run a worker",
		Long: "Run a worker: it leases jobs from the server and runs them on this host.\n" +
			"It reads its worker token from CASEBOX_WORKER_TOKEN, and a GitHub token for cloning from GITHUB_TOKEN.\n" +
			"Model API keys stay in this host's environment; the server never sees them.\n" +
			"Sandboxes come from CASEBOX_SANDBOX (docker, kiln or daytona; default docker), at most CASEBOX_SANDBOX_CONCURRENCY\n" +
			"at once (default 2).\n" +
			"Steering classification needs an analysis model: CASEBOX_ANALYSIS_PROVIDER (anthropic or openai, any\n" +
			"OpenAI-compatible API), CASEBOX_ANALYSIS_MODEL, and optionally CASEBOX_ANALYSIS_BASE_URL and CASEBOX_ANALYSIS_API_KEY\n" +
			"(default ANTHROPIC_API_KEY or OPENAI_API_KEY).",
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
			mirrors, err := config.Path("worker", "mirrors")
			if err != nil {
				return err
			}
			client := api.New(server, token)
			jobs := worker.RepoJobs{Client: client, MirrorRoot: mirrors, GitHubToken: os.Getenv("GITHUB_TOKEN")}
			steer := worker.Steering{Client: client, MirrorRoot: mirrors, GitHubToken: os.Getenv("GITHUB_TOKEN"), Concurrency: 4}
			handlers := map[string]worker.Handler{
				"entire.fetch": jobs.Entire,
				"gitai.fetch":  jobs.GitAI,
				"steering.pr":  steer.PR,
			}
			// Only a worker with an analysis model leases classification jobs.
			out := cmd.OutOrStdout()
			model, err := analysis.FromEnv()
			switch {
			case err == nil:
				steer.Model = analysis.New(model)
				handlers["steering.classify"] = steer.Classify
				fmt.Fprintf(out, "Analysis model: %s at %s; this worker classifies steering.\n", model, model.BaseURL)
			case errors.Is(err, analysis.ErrNotConfigured):
				fmt.Fprintln(out, "Analysis model: not configured, so this worker does not classify steering. Set CASEBOX_ANALYSIS_PROVIDER and CASEBOX_ANALYSIS_MODEL to enable it.")
			default:
				fmt.Fprintf(out, "Analysis model: %v. This worker does not classify steering.\n", err)
			}
			// Only a worker whose sandbox provider answers prepares environments.
			if provider, name, err := providers.Available(cmd.Context()); err == nil {
				env := worker.Environments{Provider: provider, Name: name, MirrorRoot: mirrors, GitHubToken: os.Getenv("GITHUB_TOKEN")}
				handlers["env.build"] = env.Build
				fmt.Fprintf(out, "Sandboxes: %s; this worker prepares environments.\n", name)
			} else {
				fmt.Fprintf(out, "Sandboxes: %v. This worker does not prepare environments.\n", err)
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
			fmt.Fprintf(out, "Worker %s is running against %s. Ctrl-C stops it.\n", id, server)
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
