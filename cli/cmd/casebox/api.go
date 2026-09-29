package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/skill"
)

func newAPICommand() *cobra.Command {
	var data string
	cmd := &cobra.Command{
		Use:   "api <GET|POST|PUT|DELETE> <path>",
		Short: "Call the Casebox API and print its JSON",
		Long: "Call a route under /api/v1 with this machine's login (or CASEBOX_SERVER and CASEBOX_TOKEN when set) and print\n" +
			"the answer as indented JSON. The API reference lists the routes. The agent skill reads Casebox with it.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			method, path := strings.ToUpper(args[0]), args[1]
			if !strings.HasPrefix(path, "/api/v1/") {
				return errors.New("the path starts with /api/v1/, such as /api/v1/steering/report")
			}
			_, cfg, _ := repoConfig(cmd.Context())
			client, err := tokenClient(cfg)
			if err != nil {
				return err
			}
			var body any
			if data != "" {
				if err := json.Unmarshal([]byte(data), &body); err != nil {
					return fmt.Errorf("--data is not JSON: %w", err)
				}
			}
			var out json.RawMessage
			if err := client.Do(cmd.Context(), method, path, body, &out); err != nil {
				return err
			}
			if len(out) == 0 {
				return nil
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, out, "", "  "); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), pretty.String())
			return nil
		},
	}
	cmd.Flags().StringVar(&data, "data", "", "a JSON body for POST and PUT")
	return cmd
}

func newSkillCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "skill", Short: "Install Casebox's agent skill"}
	var agent string
	install := &cobra.Command{
		Use:   "install --agent claude-code|cursor",
		Short: "Install the skill that lets a coding agent read the steering report, explain a verdict and open a case",
		Long: "Write Casebox's agent skill where the agent reads user-level skills or rules: ~/.claude/skills/casebox/SKILL.md\n" +
			"for Claude Code, ~/.cursor/rules/casebox.mdc for Cursor. Running it again replaces the file with this version's.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			var path, text string
			switch agent {
			case "claude-code":
				path, text = filepath.Join(home, ".claude", "skills", "casebox", "SKILL.md"), skill.Text
			case "cursor":
				// Cursor's rule front matter: the description decides when the agent reads it.
				_, rest, _ := strings.Cut(strings.TrimPrefix(skill.Text, "---\n"), "\n---\n")
				desc := ""
				for _, line := range strings.Split(skill.Text, "\n") {
					if d, ok := strings.CutPrefix(line, "description: "); ok {
						desc = d
					}
				}
				path, text = filepath.Join(home, ".cursor", "rules", "casebox.mdc"), fmt.Sprintf("---\ndescription: %s\nalwaysApply: false\n---\n%s", desc, rest)
			default:
				return fmt.Errorf("--agent %q: use claude-code or cursor", agent)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s.\n", path)
			return nil
		},
	}
	install.Flags().StringVar(&agent, "agent", "", "claude-code or cursor")
	_ = install.MarkFlagRequired("agent")
	cmd.AddCommand(install)
	return cmd
}

// tokenClient is CASEBOX_TOKEN's client when set, else this machine's login.
func tokenClient(cfg repo.Config) (*api.Client, error) {
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
