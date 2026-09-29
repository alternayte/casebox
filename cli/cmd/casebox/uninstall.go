package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/hooks"
)

func newUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove Casebox's hooks and telemetry settings from the agents on this machine",
		Long: "Remove the capture hooks casebox init and casebox join wrote for Claude Code, Codex and the Cursor CLI, and the\n" +
			"native telemetry settings that send Claude Code's and Codex's telemetry to the server. Every other hook and\n" +
			"setting stays. The server keeps what it received; ~/.casebox keeps this machine's login and spool, and you can\n" +
			"delete that directory and the casebox binary afterwards.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			for _, t := range hooks.Targets(home) {
				if installed, _, _ := hooks.Installed(t); !installed {
					continue
				}
				if err := hooks.Uninstall(t); err != nil {
					return fmt.Errorf("remove the %s hooks: %w", t.Agent, err)
				}
				fmt.Fprintf(out, "%s: hooks removed from %s.\n", t.Agent, tilde(home, t.Path))
			}
			claude := filepath.Join(home, ".claude", "settings.json")
			if removed, err := hooks.RemoveClaudeTelemetry(claude); err != nil {
				return err
			} else if removed {
				fmt.Fprintf(out, "claude-code: telemetry settings removed from %s.\n", tilde(home, claude))
			}
			codex := filepath.Join(home, ".codex", "config.toml")
			if removed, err := hooks.RemoveCodexTelemetry(codex); err != nil {
				return err
			} else if removed {
				fmt.Fprintf(out, "codex: telemetry block removed from %s.\n", tilde(home, codex))
			}
			fmt.Fprintln(out, "Casebox no longer captures anything on this machine.")
			return nil
		},
	}
}
