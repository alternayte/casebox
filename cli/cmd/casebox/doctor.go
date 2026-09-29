package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/hooks"
	"github.com/alternayte/casebox/cli/internal/pipeline"
	"github.com/alternayte/casebox/cli/internal/repo"
)

func newDoctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Show each capture source, agent coverage and the spool",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			problems := 0
			report := func(ok bool, format string, args ...any) {
				mark := "ok  "
				if !ok {
					mark, problems = "fix ", problems+1
				}
				fmt.Fprintf(out, "  %s %s\n", mark, fmt.Sprintf(format, args...))
			}

			step(out, "Machine")
			report(!config.Paused(), "capture %s", map[bool]string{true: "is paused (casebox resume)", false: "is on"}[config.Paused()])
			creds, err := config.LoadCredentials()
			report(err == nil && creds.IngestToken != "", "server %s", firstOf(creds.Server, "not connected (casebox init or join)"))
			if err == nil && creds.IngestToken != "" {
				client, _ := ingestClient()
				mode, err := pipeline.RefreshMode(ctx, client)
				report(err == nil && mode != "", "prompt mode %s", firstOf(mode, errText(err, "not chosen (an Admin runs casebox init)")))
			}

			step(out, "This repository")
			if cwd, err := os.Getwd(); err == nil {
				root, _, err := repo.LoadConfig(ctx, cwd)
				report(err == nil, "%s", map[bool]string{true: "enrolled: " + root, false: "not enrolled: capture is off here (casebox init)"}[err == nil])
			}

			step(out, "Agents")
			for _, t := range hooks.Targets(home) {
				if !agentPresent(home, t.Agent) {
					fmt.Fprintf(out, "  -    %s not found\n", t.Agent)
					continue
				}
				installed, binaryOK, err := hooks.Installed(t)
				switch {
				case err != nil:
					report(false, "%s: %v", t.Agent, err)
				case !installed:
					report(false, "%s: hooks are not installed (casebox join)", t.Agent)
				case !binaryOK:
					report(false, "%s: hooks point at a casebox binary that no longer exists (casebox join)", t.Agent)
				default:
					report(true, "%s: hooks installed", t.Agent)
				}
				if t.NeedsTrust && installed {
					missing, err := hooks.CodexUntrusted(t, filepath.Join(home, ".codex", "config.toml"))
					report(err == nil && len(missing) == 0, "%s: %s", t.Agent, map[bool]string{true: "hooks trusted", false: "hooks not trusted yet: open Codex and approve them in /hooks"}[err == nil && len(missing) == 0])
				}
			}
			fmt.Fprintln(out, "  Coverage: Claude Code and Codex give full transcripts, hooks and telemetry; Cursor CLI gives hooks only.")

			step(out, "Spool")
			s, err := openSpool()
			if err != nil {
				return err
			}
			defer s.Close()
			st, err := s.Stats(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "  %d sessions, %d events waiting, %d uploaded, %.1f MB of 500 MB\n", st.Sessions, st.Pending, st.Acked, float64(st.SizeBytes)/(1<<20))
			report(st.Rejected == 0, "%d events refused by the server", st.Rejected)
			if dir, err := config.Path("queue"); err == nil {
				entries, _ := os.ReadDir(dir)
				waiting := 0
				for _, e := range entries {
					if strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
						waiting++
					}
				}
				report(waiting < 100, "%d hook calls waiting to be processed", waiting)
			}

			if lines := lastLines("errors.log", 5); len(lines) > 0 {
				step(out, "Recent background errors (~/.casebox/errors.log)")
				for _, l := range lines {
					fmt.Fprintf(out, "  %s\n", l)
				}
			}
			if problems > 0 {
				fmt.Fprintf(out, "\n%d things to fix.\n", problems)
			} else {
				fmt.Fprintln(out, "\nEverything is set up.")
			}
			return nil
		},
	}
}

func errText(err error, fallback string) string {
	if err != nil {
		return "unknown: " + err.Error()
	}
	return fallback
}

func lastLines(name string, n int) []string {
	path, err := config.Path(name)
	if err != nil {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return lines
}
