package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/hooks"
	"github.com/alternayte/casebox/cli/internal/pipeline"
	"github.com/alternayte/casebox/cli/internal/repo"
)

type me struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	OrgID       string `json:"orgId"`
}

type orgSettings struct {
	PromptMode      *string        `json:"promptMode"`
	K               int            `json:"k"`
	PseudonymPeriod string         `json:"pseudonymPeriod"`
	Budgets         map[string]any `json:"budgets"`
}

var promptModes = []struct{ name, explain string }{
	{"off", "Keep structure only: counts, timings, tool calls and edits. Steering is counted, not read."},
	{"redacted", "Keep prompts and responses after redaction and pseudonymization. Suggested: corrections are classified by content."},
	{"full", "Also keep tool output and file contents the agent saw. Richer steering cases; more data leaves each machine."},
}

func newInitCommand() *cobra.Command {
	var server, workspace, mode, jiraURL string
	var jiraProjects []string
	var githubIssues bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up this repository and this machine (admin)",
		Long: "Connect to the server, choose the team's prompt mode, create the workspace, write .casebox/casebox.yml,\n" +
			"and install capture hooks for the agents on this machine. Every step is safe to run again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out, in := cmd.Context(), cmd.OutOrStdout(), bufio.NewReader(cmd.InOrStdin())
			root, cfg, err := repoConfig(ctx)
			if err != nil && !errors.Is(err, repo.ErrNotEnrolled) {
				return err
			}
			server = firstOf(server, cfg.Server, "http://localhost:8080")
			state := repo.Current(ctx, root)
			if state.Repo == "" {
				return errors.New("this repository has no origin remote; Casebox names repositories by their remote, such as github.com/acme/app")
			}
			workspace = firstOf(workspace, cfg.Workspace, workspaceName(state.Repo))

			client, who, err := login(ctx, server, out)
			if err != nil {
				return err
			}
			if who.Role != "admin" && who.Role != "owner" {
				return fmt.Errorf("casebox init needs an Admin; %s is a %s. Ask an Admin to run it, then run casebox join", who.DisplayName, who.Role)
			}

			step(out, "Prompt mode")
			var settings orgSettings
			if err := client.Do(ctx, http.MethodGet, "/api/v1/org/", nil, &struct {
				Settings *orgSettings `json:"settings"`
			}{&settings}); err != nil {
				return err
			}
			if settings.PromptMode == nil || mode != "" {
				chosen, err := choosePromptMode(mode, out, in)
				if err != nil {
					return err
				}
				settings.PromptMode = &chosen
				if err := client.Do(ctx, http.MethodPut, "/api/v1/org/settings", settings, nil); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "  The team's prompt mode is %s.\n", *settings.PromptMode)

			step(out, "Workspace")
			err = client.Do(ctx, http.MethodPost, "/api/v1/workspaces/", map[string]string{"name": workspace}, nil)
			if err != nil && api.StatusOf(err) != http.StatusConflict {
				return err
			}
			if err := client.Do(ctx, http.MethodPost, "/api/v1/workspaces/"+workspace+"/repos", map[string]string{"repo": state.Repo}, nil); err != nil {
				return err
			}
			fmt.Fprintf(out, "  %s is in workspace %s.\n", state.Repo, workspace)

			if err := writeConfig(root, server, workspace, state.Repo); err != nil {
				return err
			}
			fmt.Fprintf(out, "  Wrote %s. Commit it when your team joins: casebox join reads it.\n", repo.ConfigPath)

			if err := connectIntegrations(ctx, client, cmd, in, out, jiraURL, jiraProjects, githubIssues); err != nil {
				return err
			}
			return setUpMachine(ctx, client, server, *settings.PromptMode, out)
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "the Casebox server (default: casebox.yml, then http://localhost:8080)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "the workspace name (default: the repository name)")
	cmd.Flags().StringVar(&mode, "prompt-mode", "", "off, redacted or full; without it, init asks")
	cmd.Flags().StringVar(&jiraURL, "jira-url", "", "the Jira Data Center URL, such as https://jira.example.com")
	cmd.Flags().StringSliceVar(&jiraProjects, "jira-project", nil, "a Jira project key, such as PAY; repeat for more")
	cmd.Flags().BoolVar(&githubIssues, "github-issues", false, "track GitHub Issues as work items")
	return cmd
}

// connectIntegrations connects GitHub (a fine-grained token) and Jira Data Center (a personal
// access token). Tokens come from CASEBOX_GITHUB_TOKEN and CASEBOX_JIRA_TOKEN, or a hidden prompt,
// never from flags, which end up in shell history. Each step can be skipped and run again later.
func connectIntegrations(ctx context.Context, client *api.Client, cmd *cobra.Command, in *bufio.Reader, out io.Writer, jiraURL string, jiraProjects []string, githubIssues bool) error {
	step(out, "GitHub")
	token := os.Getenv("CASEBOX_GITHUB_TOKEN")
	if token == "" {
		token = secretPrompt(out, in, "  Fine-grained token with read access to contents, pull requests, issues and checks (Enter skips): ")
	}
	if token == "" {
		fmt.Fprintln(out, "  Skipped. Casebox sees no pull requests until GitHub is connected.")
	} else {
		if err := client.Do(ctx, http.MethodPut, "/api/v1/integrations/github", map[string]any{"mode": "token", "token": token, "issues": githubIssues}, nil); err != nil {
			return fmt.Errorf("connect GitHub: %w", err)
		}
		fmt.Fprintln(out, "  Connected. The server polls every 5 minutes.")
	}

	step(out, "Jira Data Center")
	if jiraURL == "" {
		jiraURL = linePrompt(out, in, "  Jira URL (Enter skips): ")
	}
	if jiraURL == "" {
		fmt.Fprintln(out, "  Skipped.")
		return nil
	}
	if len(jiraProjects) == 0 {
		if keys := linePrompt(out, in, "  Project keys, comma-separated (such as PAY,OPS): "); keys != "" {
			jiraProjects = strings.Split(keys, ",")
		}
	}
	jiraToken := os.Getenv("CASEBOX_JIRA_TOKEN")
	if jiraToken == "" {
		jiraToken = secretPrompt(out, in, "  Personal access token: ")
	}
	if len(jiraProjects) == 0 || jiraToken == "" {
		return errors.New("Jira needs project keys and a personal access token; run casebox init again to connect it")
	}
	if err := client.Do(ctx, http.MethodPut, "/api/v1/integrations/jira", map[string]any{"url": jiraURL, "token": jiraToken, "projects": jiraProjects}, nil); err != nil {
		return fmt.Errorf("connect Jira: %w", err)
	}
	fmt.Fprintln(out, "  Connected. The server polls every 5 minutes.")
	return nil
}

func linePrompt(out io.Writer, in *bufio.Reader, prompt string) string {
	fmt.Fprint(out, prompt)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

// secretPrompt reads without echo on a terminal, and a plain line otherwise (tests, pipes).
func secretPrompt(out io.Writer, in *bufio.Reader, prompt string) string {
	fmt.Fprint(out, prompt)
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		secret, err := term.ReadPassword(fd)
		fmt.Fprintln(out)
		if err == nil {
			return strings.TrimSpace(string(secret))
		}
	}
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func newJoinCommand() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Set up this machine for an enrolled repository",
		Long:  "Log in, install capture hooks for the agents on this machine, and import your recent history.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			_, cfg, err := repoConfig(ctx)
			if err != nil {
				return err
			}
			if cfg.Server == "" {
				return fmt.Errorf("%s names no server; ask an Admin to run casebox init", repo.ConfigPath)
			}
			client, _, err := login(ctx, cfg.Server, out)
			if err != nil {
				return err
			}
			var org struct {
				Settings orgSettings `json:"settings"`
			}
			if err := client.Do(ctx, http.MethodGet, "/api/v1/org/", nil, &org); err != nil {
				return err
			}
			if org.Settings.PromptMode == nil {
				return errors.New("an Admin has not chosen the team's prompt mode yet; ask them to run casebox init")
			}
			if err := setUpMachine(ctx, client, cfg.Server, *org.Settings.PromptMode, out); err != nil {
				return err
			}
			step(out, "History")
			importCmd := newImportCommand()
			importCmd.SetOut(out)
			importCmd.SetContext(ctx)
			_ = importCmd.Flags().Set("days", fmt.Sprint(days))
			return importCmd.RunE(importCmd, nil)
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "how many days of history to import")
	return cmd
}

// setUpMachine gives this machine an ingest token, installs the hooks and sets up native telemetry.
func setUpMachine(ctx context.Context, client *api.Client, server, mode string, out io.Writer) error {
	step(out, "This machine")
	creds, _ := config.LoadCredentials()
	if creds.IngestToken == "" || creds.Server != server {
		var issued struct {
			Secret string `json:"secret"`
		}
		if err := client.Do(ctx, http.MethodPost, "/api/v1/devices", nil, &issued); err != nil {
			return err
		}
		creds.IngestToken = issued.Secret
	}
	creds.Server, creds.CLIToken = server, client.Token
	if err := config.SaveCredentials(creds); err != nil {
		return err
	}
	if _, err := pipeline.RefreshMode(ctx, api.New(server, creds.IngestToken)); err != nil {
		return err
	}
	fmt.Fprintln(out, "  This machine has its own ingest token in ~/.casebox/credentials.json.")

	step(out, "Agents")
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	telemetry := hooks.Telemetry{Server: server, IngestToken: creds.IngestToken, LogPrompts: mode != pipeline.ModeOff}
	found := 0
	for _, t := range hooks.Targets(home) {
		if !agentPresent(home, t.Agent) {
			continue
		}
		found++
		if err := hooks.Install(t, exe); err != nil {
			return fmt.Errorf("install the %s hooks: %w", t.Agent, err)
		}
		fmt.Fprintf(out, "  %s: hooks installed in %s.\n", t.Agent, tilde(home, t.Path))
		switch t.Agent {
		case hooks.ClaudeCode:
			if err := hooks.SetClaudeTelemetry(t.Path, telemetry); err != nil {
				return err
			}
			fmt.Fprintln(out, "  claude-code: native telemetry goes to the server.")
		case hooks.Codex:
			switch err := hooks.SetCodexTelemetry(filepath.Join(home, ".codex", "config.toml"), telemetry); {
			case errors.Is(err, hooks.ErrCodexOtelTaken):
				fmt.Fprintf(out, "  codex: %v\n", err)
			case err != nil:
				return err
			default:
				fmt.Fprintln(out, "  codex: native telemetry goes to the server.")
			}
			fmt.Fprintln(out, "  codex: "+cbx.Line(cbx.CodexHookTrust, "Codex runs a new hook only after you trust it. Open Codex and approve the Casebox hooks in /hooks."))
		}
	}
	if found == 0 {
		fmt.Fprintln(out, "  No Claude Code, Codex or Cursor CLI found on this machine. Install one, then run this again.")
	}
	fmt.Fprintln(out, "\nCapture is on for enrolled repositories. casebox pause stops it at any time.")
	return nil
}

// login returns a client with a CLI token, from the credentials or from a new device login.
func login(ctx context.Context, server string, out io.Writer) (*api.Client, me, error) {
	step(out, "Log in to "+server)
	var who me
	if creds, err := config.LoadCredentials(); err == nil && creds.Server == server && creds.CLIToken != "" {
		client := api.New(server, creds.CLIToken)
		if err := client.Do(ctx, http.MethodGet, "/api/v1/me", nil, &who); err == nil {
			fmt.Fprintf(out, "  Logged in as %s (%s).\n", who.DisplayName, who.Role)
			return client, who, nil
		}
	}
	anon := api.New(server, "")
	code, err := anon.StartDeviceLogin(ctx)
	if err != nil {
		return nil, who, fmt.Errorf("start the login: %w", err)
	}
	link := code.VerificationURI + "?code=" + code.UserCode
	fmt.Fprintf(out, "  Open %s and approve the code %s.\n", link, code.UserCode)
	openBrowser(link)
	token, err := anon.WaitForDeviceToken(ctx, code)
	if err != nil {
		return nil, who, err
	}
	client := api.New(server, token.AccessToken)
	if err := client.Do(ctx, http.MethodGet, "/api/v1/me", nil, &who); err != nil {
		return nil, who, err
	}
	creds, _ := config.LoadCredentials()
	if creds.Server != server {
		creds = config.Credentials{}
	}
	creds.Server, creds.OrgID, creds.CLIToken = server, token.OrgID, token.AccessToken
	if err := config.SaveCredentials(creds); err != nil {
		return nil, who, err
	}
	fmt.Fprintf(out, "  Logged in as %s (%s).\n", who.DisplayName, who.Role)
	return client, who, nil
}

func choosePromptMode(flag string, out io.Writer, in *bufio.Reader) (string, error) {
	if flag != "" {
		for _, m := range promptModes {
			if m.name == flag {
				return flag, nil
			}
		}
		return "", fmt.Errorf("--prompt-mode is %q; use off, redacted or full", flag)
	}
	fmt.Fprintln(out, "  Choose what agent sessions keep. This applies to the whole team; there is no default.")
	for i, m := range promptModes {
		fmt.Fprintf(out, "    %d. %-8s %s\n", i+1, m.name, m.explain)
	}
	for {
		fmt.Fprint(out, "  Mode [1-3]: ")
		line, err := in.ReadString('\n')
		answer := strings.TrimSpace(line)
		for i, m := range promptModes {
			if answer == fmt.Sprint(i+1) || answer == m.name {
				return m.name, nil
			}
		}
		if err != nil {
			return "", errors.New("no prompt mode chosen; run casebox init --prompt-mode redacted to choose without a prompt")
		}
	}
}

func writeConfig(root, server, workspace, repoName string) error {
	path := filepath.Join(root, repo.ConfigPath)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(`# yaml-language-server: $schema=https://casebox-docs.pages.dev/schema/casebox.schema.json
version: 1
server: %s
workspace: %s
repos:
  - %s
harness:
  globs: [AGENTS.md, CLAUDE.md, ".cursor/rules/**", ".claude/skills/**", ".agents/skills/**", ".mcp.json"]
capture:
  redact: []   # extra secret patterns (regular expressions) to remove before anything leaves a machine
`, server, workspace, repoName)
	return os.WriteFile(path, []byte(content), 0o644)
}

func repoConfig(ctx context.Context) (string, repo.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", repo.Config{}, err
	}
	root, cfg, err := repo.LoadConfig(ctx, cwd)
	if root == "" {
		return "", cfg, errors.New("run this inside a git repository")
	}
	return root, cfg, err
}

func agentPresent(home, agent string) bool {
	bins := map[string]string{hooks.ClaudeCode: "claude", hooks.Codex: "codex", hooks.Cursor: "cursor-agent"}
	dirs := map[string]string{hooks.ClaudeCode: ".claude", hooks.Codex: ".codex", hooks.Cursor: ".cursor"}
	if _, err := exec.LookPath(bins[agent]); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(home, dirs[agent]))
	return err == nil
}

func workspaceName(repoName string) string {
	name := strings.ToLower(repoName[strings.LastIndex(repoName, "/")+1:])
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func step(out io.Writer, title string) { fmt.Fprintf(out, "\n%s\n", title) }

func tilde(home, path string) string {
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + filepath.ToSlash(rel)
	}
	return path
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
