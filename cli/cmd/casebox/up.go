package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/buildinfo"
	"github.com/alternayte/casebox/cli/internal/stack"
)

func newUpCommand() *cobra.Command {
	opts := stack.Options{Port: 8080}
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start a local server, QueueBox and Postgres in Docker",
		Long: "Start a local Casebox server, QueueBox and Postgres in Docker, and wait until they are healthy.\n" +
			"Running it again keeps the data and the admin password.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Version = buildinfo.Version
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Starting Casebox, QueueBox and Postgres…")
			env, err := stack.Up(cmd.Context(), opts, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\nCasebox is running at http://localhost:%d\n", opts.Port)
			fmt.Fprintf(out, "Admin password: %s\n", env["CASEBOX_ADMIN_PASSWORD"])
			fmt.Fprintln(out, "Keys are in database mode: the master key sits next to the data. Use this stack for trials only.")
			if opts.Demo {
				fmt.Fprintln(out, "Demo data: a synthetic team of five in workspace demo, with a steering report, cases, an evaluation and a proposal.")
				fmt.Fprintln(out, "It loads only while the organisation has no session of its own.")
			}
			fmt.Fprintln(out, "Next: run casebox init in a repository.")
			return nil
		},
	}
	cmd.Flags().IntVar(&opts.Port, "port", opts.Port, "the port of the web UI and API")
	cmd.Flags().StringVar(&opts.Image, "image", "", "the server image to run instead of the one that matches this CLI")
	cmd.Flags().BoolVar(&opts.Demo, "demo", false, "load a synthetic team to see every page before connecting anything")
	return cmd
}

func newDownCommand() *cobra.Command {
	var volumes bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the local server, QueueBox and Postgres",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := stack.Down(cmd.Context(), volumes, cmd.ErrOrStderr()); err != nil {
				return err
			}
			if volumes {
				fmt.Fprintln(cmd.OutOrStdout(), "Casebox is stopped, and its data is deleted.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Casebox is stopped. Its data stays; casebox up starts it again.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&volumes, "volumes", false, "also delete the database")
	return cmd
}
