package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/propose"
)

func newApplyCommand() *cobra.Command {
	var commit bool
	var agent string
	cmd := &cobra.Command{
		Use:   "apply <id>",
		Short: "Write an approved proposal into this repository",
		Long: "Write an approved proposal into this working tree.\n" +
			"Without --commit it stays private. New instruction bullets become their own Cursor rule, and a new skill,\n" +
			"rule or MCP config is written as itself. Every file goes into .git/info/exclude: git status stays clean,\n" +
			"and your team sees nothing. With --commit it edits the shared files for you to commit.\n" +
			"A code note starts your agent (the Cursor CLI, or Claude Code; --agent picks) with the note's prompt; you\n" +
			"steer it and review its diff like any other.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, _, err := repoConfig(ctx)
			if err != nil {
				return err
			}
			client, err := cliClient()
			if err != nil {
				return err
			}
			d, err := getProposal(ctx, client, args[0])
			if err != nil {
				return err
			}
			p := d.Proposal
			if p.Status != "approved" && p.Status != "applied" {
				return fmt.Errorf("proposal %s is %s; approve it first with casebox proposals approve %s", p.ID, p.Status, p.ID)
			}
			out := cmd.OutOrStdout()
			mode := "private"
			switch {
			case d.Note != nil:
				mode = "commit"
				if err := runAgent(ctx, root, agent, d.Note.Prompt, cmd.InOrStdin(), out, cmd.ErrOrStderr()); err != nil {
					return err
				}
				fmt.Fprintln(out, "Review the agent's diff, then commit it like any other change.")
			case commit:
				mode = "commit"
				if err := applyShared(root, p.ID, d.Edits, out); err != nil {
					return err
				}
			default:
				if err := applyPrivate(ctx, root, p.ID, p.PatternTitle, d.Edits, out); err != nil {
					return err
				}
			}
			if err := client.Do(ctx, http.MethodPost, "/api/v1/proposals/"+url.PathEscape(p.ID)+"/applied", map[string]string{"mode": mode}, nil); err != nil {
				return fmt.Errorf("the change is written, but the server did not record it: %w", err)
			}
			fmt.Fprintln(out, "Casebox compares this pattern's corrections 30 days before and after today.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&commit, "commit", false, "edit the shared files for your team instead of writing a private file")
	cmd.Flags().StringVar(&agent, "agent", "", "the agent that carries out a code note: cursor or claude (default: the one installed)")
	return cmd
}

// currentFiles reads the files the edits name from the working tree; a missing file is left out.
func currentFiles(root string, edits []propose.Edit) map[string]string {
	files := map[string]string{}
	for _, e := range edits {
		if body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.File))); err == nil {
			files[e.File] = string(body)
		}
	}
	return files
}

func applyPrivate(ctx context.Context, root, id, title string, edits []propose.Edit, out io.Writer) error {
	tracked := func(path string) bool {
		c := exec.CommandContext(ctx, "git", "ls-files", "--error-unmatch", "--", path)
		c.Dir = root
		return c.Run() == nil
	}
	written, err := propose.Private(id, title, edits, currentFiles(root, edits), tracked)
	if err != nil {
		return err
	}
	exclude, err := gitPath(ctx, root, "info/exclude")
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(written) {
		if err := writeFile(root, name, written[name]); err != nil {
			return err
		}
		if err := excludeLocally(exclude, name); err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote %s privately: it is in .git/info/exclude, so only this machine has it.\n", name)
	}
	fmt.Fprintf(out, "Your agent reads it from its next session. casebox apply %s --commit shares it with your team later.\n", id)
	return nil
}

func applyShared(root, id string, edits []propose.Edit, out io.Writer) error {
	changed, err := propose.Apply(currentFiles(root, edits), edits, func(string) bool { return true })
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(changed) {
		if changed[name] == nil {
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			fmt.Fprintf(out, "Removed %s.\n", name)
			continue
		}
		if err := writeFile(root, name, *changed[name]); err != nil {
			return err
		}
		fmt.Fprintf(out, "Changed %s.\n", name)
	}
	// A private rule of the same proposal is now in the shared files.
	private := filepath.Join(root, filepath.FromSlash(propose.PrivateRule(id)))
	if err := os.Remove(private); err == nil {
		fmt.Fprintf(out, "Removed the private %s.\n", propose.PrivateRule(id))
	}
	fmt.Fprintln(out, "Commit the change and open a pull request as usual.")
	return nil
}

// runAgent starts the person's agent in the repository with the prompt, attached to this terminal.
func runAgent(ctx context.Context, root, agent, prompt string, in io.Reader, out, errOut io.Writer) error {
	candidates := map[string][]string{"cursor": {"cursor-agent", "agent"}, "claude": {"claude"}}
	order := []string{"cursor", "claude"}
	if agent != "" {
		if _, ok := candidates[agent]; !ok {
			return fmt.Errorf("--agent %s: use cursor or claude", agent)
		}
		order = []string{agent}
	}
	for _, a := range order {
		for _, bin := range candidates[a] {
			path, err := exec.LookPath(bin)
			if err != nil {
				continue
			}
			fmt.Fprintf(out, "Starting %s with the note's prompt. Steer it as usual; exit it when the change is done.\n", bin)
			c := exec.CommandContext(ctx, path, prompt)
			c.Dir, c.Stdin, c.Stdout, c.Stderr = root, in, out, errOut
			return c.Run()
		}
	}
	return errors.New("neither the Cursor CLI (cursor-agent) nor Claude Code (claude) is on PATH; give the note's prompt to your agent by hand (casebox proposals show)")
}

func gitPath(ctx context.Context, root, name string) (string, error) {
	c := exec.CommandContext(ctx, "git", "rev-parse", "--git-path", name)
	c.Dir = root
	b, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("find .git/%s: %w", name, err)
	}
	p := strings.TrimSpace(string(b))
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return p, nil
}

// excludeLocally adds a path to .git/info/exclude once: git ignores it on this machine only.
func excludeLocally(exclude, name string) error {
	line := "/" + name
	data, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return err
	}
	text := string(data)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return os.WriteFile(exclude, []byte(text+line+"\n"), 0o644)
}

func writeFile(root, name, content string) error {
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
