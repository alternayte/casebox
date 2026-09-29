package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/buildinfo"
	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/worker"
)

func newWorkerCommand() *cobra.Command {
	var server, id string
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run a worker",
		Long: "Run a worker: it leases jobs from the server and runs them on this host.\n" +
			"It reads its worker token from CASEBOX_WORKER_TOKEN, and a GitHub token for cloning from GITHUB_TOKEN.\n" +
			"Model API keys stay in this host's environment; the server never sees them.",
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
			w := &worker.Worker{
				Client:  client,
				ID:      id,
				Version: buildinfo.Version,
				Log:     cmd.ErrOrStderr(),
				Handlers: map[string]worker.Handler{
					"entire.fetch": jobs.Entire,
					"gitai.fetch":  jobs.GitAI,
				},
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
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
