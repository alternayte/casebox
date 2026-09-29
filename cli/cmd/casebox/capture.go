package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/detach"
	"github.com/alternayte/casebox/cli/internal/hooks"
	"github.com/alternayte/casebox/cli/internal/importer"
	"github.com/alternayte/casebox/cli/internal/pipeline"
	"github.com/alternayte/casebox/cli/internal/spool"
	"github.com/alternayte/casebox/cli/internal/upload"
)

func init() {
	hooks.Background = func(args ...string) error { return detach.Start(args...) }
}

// newHookCommand is what the agents' hook configurations run. It prints nothing, because some
// agents feed a hook's output to the model, and it always exits 0, so it never blocks the agent.
func newHookCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "hook <agent> <event>",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := hooks.Enqueue(args[0], args[1], os.Stdin); err != nil {
				logError("hook "+args[0]+" "+args[1], err)
			}
			return nil
		},
	}
}

// newCaptureCommand holds the background work the hooks start.
func newCaptureCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "capture", Hidden: true}

	var agent, path, cwd string
	transcript := &cobra.Command{
		Use:  "transcript",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := importTranscript(cmd.Context(), agent, path, cwd); err != nil {
				logError("capture transcript", err)
			}
			return nil
		},
	}
	transcript.Flags().StringVar(&agent, "agent", "", "")
	transcript.Flags().StringVar(&path, "path", "", "")
	transcript.Flags().StringVar(&cwd, "cwd", "", "")

	uploadCmd := &cobra.Command{
		Use:  "upload",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := backgroundUpload(cmd.Context()); err != nil {
				logError("capture upload", err)
			}
			return nil
		},
	}
	process := &cobra.Command{
		Use:  "process",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			if err := hooks.Process(ctx, func(err error) { logError("capture process", err) }); err != nil {
				logError("capture process", err)
			}
			return nil
		},
	}
	cmd.AddCommand(transcript, uploadCmd, process)
	return cmd
}

func importTranscript(ctx context.Context, agent, path, cwd string) error {
	parsed, err := importer.ParseFile(path, agent)
	if err != nil || parsed.Subagent {
		return err
	}
	if parsed.Cwd == "" {
		parsed.Cwd = cwd
	}
	r, err := pipeline.Open(ctx, parsed.Cwd)
	if err != nil {
		return nil
	}
	s, err := openSpool()
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Add(ctx, r.Session(parsed.Session), r.Events(parsed.Events, pipeline.Mode())); err != nil {
		return err
	}
	return detach.Start("capture", "upload")
}

// backgroundUpload runs at most once every 30 seconds; the next hook call starts it again.
func backgroundUpload(ctx context.Context) error {
	s, err := openSpool()
	if err != nil {
		return err
	}
	defer s.Close()
	var last time.Time
	if _, err := s.State(ctx, "upload:last", &last); err != nil {
		return err
	}
	if time.Since(last) < 30*time.Second {
		return nil
	}
	if err := s.SetState(ctx, "upload:last", time.Now().UTC()); err != nil {
		return err
	}
	client, err := ingestClient()
	if err != nil {
		return err
	}
	_, _ = pipeline.RefreshMode(ctx, client)
	_, err = upload.Run(ctx, client, s)
	return err
}

func newImportCommand() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import past sessions from the session logs on this machine",
		Long: "Import the Claude Code and Codex sessions of the last days that ran in this repository, and upload them.\n" +
			"Running it again imports only what is new.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			r, err := pipeline.Open(ctx, cwd)
			if err != nil {
				return err
			}
			client, err := ingestClient()
			if err != nil {
				return err
			}
			mode, err := pipeline.RefreshMode(ctx, client)
			if err != nil {
				return fmt.Errorf("read the prompt mode from the server: %w", err)
			}
			if mode == "" {
				return upload.ErrCaptureOff
			}
			s, err := openSpool()
			if err != nil {
				return err
			}
			defer s.Close()
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}

			fmt.Fprintf(out, "Reading the last %d days of sessions in %s (prompt mode %s)…\n", days, r.State.Repo, mode)
			sum, err := importer.Import(ctx, r, s, importer.Options{Home: home, Since: time.Now().AddDate(0, 0, -days), Mode: mode})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Found %d Claude Code and %d Codex sessions, %d events.\n", sum.Sessions["claude-code"], sum.Sessions["codex"], sum.Events)
			if sum.Unsupported > 0 {
				fmt.Fprintf(out, "Skipped %d Codex sessions in the pre-2025 format.\n", sum.Unsupported)
			}
			if sum.Failed > 0 {
				fmt.Fprintf(out, "Could not read %d log files; casebox doctor lists the sources.\n", sum.Failed)
			}

			res, err := upload.Run(ctx, client, s)
			fmt.Fprintf(out, "Uploaded %d events in %d batches.\n", res.Events, res.Batches)
			if res.Rejected > 0 {
				fmt.Fprintf(out, "The server refused %d events; casebox doctor shows why.\n", res.Rejected)
			}
			return err
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "how many days of history to import")
	return cmd
}

func openSpool() (*spool.Spool, error) {
	path, err := config.Path("spool.db")
	if err != nil {
		return nil, err
	}
	return spool.Open(path, spool.DefaultCap)
}

func ingestClient() (*api.Client, error) {
	creds, err := config.LoadCredentials()
	if err != nil {
		return nil, err
	}
	if creds.IngestToken == "" {
		return nil, errors.New("this machine has no ingest token; run casebox init or casebox join")
	}
	return api.New(creds.Server, creds.IngestToken), nil
}

// logError appends to ~/.casebox/errors.log, which casebox doctor shows. Hooks and background
// work have no terminal to report to.
func logError(where string, err error) {
	path, perr := config.Path("errors.log")
	if perr != nil {
		return
	}
	f, ferr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s: %v\n", time.Now().UTC().Format(time.RFC3339), where, err)
}
