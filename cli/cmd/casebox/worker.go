package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/buildinfo"
	"github.com/alternayte/casebox/cli/internal/cases"
	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/evaluate"
	"github.com/alternayte/casebox/cli/internal/propose"
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
			"at once (default 2); environments, case validation and evaluation runs need one.\n" +
			"Evaluation runs also need a model key for the agents they run (ANTHROPIC_API_KEY, OPENAI_API_KEY or CURSOR_API_KEY).\n" +
			"Steering classification and case instructions need an analysis model: CASEBOX_ANALYSIS_PROVIDER (anthropic or openai, any\n" +
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
			client := api.New(server, token)
			handlers, err := workerHandlers(cmd.Context(), client, cmd.OutOrStdout())
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

// environ is the worker's environment as a map, for the model keys the agents read.
func environ() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// workerHandlers are the job kinds this host can run, and a line on each capability it lacks. The
// inline worker of casebox ci uses the same set.
func workerHandlers(ctx context.Context, client *api.Client, out io.Writer) (map[string]worker.Handler, error) {
	mirrors, err := config.Path("worker", "mirrors")
	if err != nil {
		return nil, err
	}
	jobs := worker.RepoJobs{Client: client, MirrorRoot: mirrors, GitHubToken: os.Getenv("GITHUB_TOKEN")}
	steer := worker.Steering{Client: client, MirrorRoot: mirrors, GitHubToken: os.Getenv("GITHUB_TOKEN"), Concurrency: 4}
	caseJobs := cases.Jobs{Client: client, MirrorRoot: mirrors, GitHubToken: os.Getenv("GITHUB_TOKEN")}
	evalJobs := evaluate.Jobs{Client: client, MirrorRoot: mirrors, GitHubToken: os.Getenv("GITHUB_TOKEN"), Env: environ()}
	handlers := map[string]worker.Handler{
		"entire.fetch": jobs.Entire,
		"gitai.fetch":  jobs.GitAI,
		"steering.pr":  steer.PR,
		"mine":         caseJobs.Mine,
	}
	// Every worker resolves harness hashes for the nightly baseline and lists the harness diet's
	// removals: both need only the mirrors.
	handlers["harness.resolve"] = evalJobs.Resolve
	proposer := propose.Jobs{Client: client, MirrorRoot: mirrors, GitHubToken: os.Getenv("GITHUB_TOKEN")}
	handlers["propose.diet"] = proposer.Diet
	// Only a worker with an analysis model leases classification jobs.
	model, err := analysis.FromEnv()
	switch {
	case err == nil:
		steer.Model = analysis.New(model)
		caseJobs.Model = steer.Model
		evalJobs.Model = steer.Model
		handlers["steering.classify"] = steer.Classify
		handlers["pattern.cluster"] = steer.Cluster
		proposer.Model = steer.Model
		handlers["propose.search"] = proposer.Search
		handlers["case.instruction"] = caseJobs.Instruction
		fmt.Fprintf(out, "Analysis model: %s at %s; this worker classifies steering and drafts case instructions.\n", model, model.BaseURL)
	case errors.Is(err, analysis.ErrNotConfigured):
		fmt.Fprintln(out, "Analysis model: not configured, so this worker does not classify steering or draft case instructions. Set CASEBOX_ANALYSIS_PROVIDER and CASEBOX_ANALYSIS_MODEL to enable it.")
	default:
		fmt.Fprintf(out, "Analysis model: %v. This worker does not classify steering or draft case instructions.\n", err)
	}
	// Only a worker whose sandbox provider answers prepares environments.
	if provider, name, err := providers.Available(ctx); err == nil {
		env := worker.Environments{Provider: provider, Name: name, MirrorRoot: mirrors, GitHubToken: os.Getenv("GITHUB_TOKEN")}
		handlers["env.build"] = env.Build
		caseJobs.Provider = provider
		handlers["case.validate"] = caseJobs.Validate
		fmt.Fprintf(out, "Sandboxes: %s; this worker prepares environments, validates cases and verifies evaluation runs.\n", name)
		evalJobs.Provider = provider
		evalJobs.ProviderName = name
		handlers["verify"] = evalJobs.Verify
		// Only a worker with a model key runs agents.
		runnable := evaluate.Available(evalJobs.Env)
		if len(runnable) > 1 {
			handlers["run"] = evalJobs.Run
			fmt.Fprintf(out, "Agents: this worker runs %s.\n", strings.Join(runnable, ", "))
		} else {
			fmt.Fprintln(out, cbx.Line(cbx.NoModelKey, "Agents: no model key (ANTHROPIC_API_KEY, OPENAI_API_KEY or CURSOR_API_KEY), so this worker runs no evaluation runs."))
		}
	} else {
		fmt.Fprintf(out, "Sandboxes: %v. This worker does not prepare environments, validate cases or run evaluations.\n", cbx.Wrap(cbx.NoSandbox, err))
	}
	return handlers, nil
}
