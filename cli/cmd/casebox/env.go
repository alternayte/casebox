package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/recipe"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox/providers"
)

// workspaceView is the server's answer about a workspace's recipe.
type workspaceView struct {
	Name         string `json:"name"`
	RecipeStatus string `json:"recipeStatus"`
	RecipeHash   string `json:"recipeHash"`
}

func newEnvCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "env", Short: "The workspace's environment recipe: draft, check, confirm"}

	draft := &cobra.Command{
		Use:   "draft",
		Short: "Print a recipe drafted from this repository's files",
		Long: "Draft the environment block of casebox.yml from the devcontainer, Dockerfile, compose files, CI workflows\n" +
			"and the languages in this repository. It prints the block; paste it into .casebox/casebox.yml and adjust it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := draftHere(cmd.Context())
			if err != nil {
				return err
			}
			text, err := d.YAML()
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), text)
			return nil
		},
	}

	check := &cobra.Command{
		Use:   "check",
		Short: "Build the recipe and run the tests at HEAD",
		Long: "Propose the recipe to the server and build it with this machine's sandbox provider (CASEBOX_SANDBOX, default\n" +
			"docker). Then copy the repository at HEAD into a sandbox, run every test command, and record the result on the server.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return envCheck(cmd.Context(), cmd.OutOrStdout())
		},
	}

	confirm := &cobra.Command{
		Use:   "confirm",
		Short: "Confirm the checked recipe (admin)",
		Long:  "Confirm the recipe whose check passed. Cases are mined only in a workspace with a confirmed recipe.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			_, cfg, client, err := envContext(ctx)
			if err != nil {
				return err
			}
			var ws workspaceView
			if err := client.Do(ctx, http.MethodGet, "/api/v1/workspaces/"+cfg.Workspace, nil, &ws); err != nil {
				return err
			}
			if cfg.Environment == nil || ws.RecipeHash != recipeHash(*cfg.Environment) {
				return errors.New("the recipe in casebox.yml is not the one the server checked; run casebox env check first")
			}
			if err := client.Do(ctx, http.MethodPost, "/api/v1/workspaces/"+cfg.Workspace+"/recipe/confirmation", map[string]string{"hash": ws.RecipeHash}, &ws); err != nil {
				return err
			}
			fmt.Fprintf(out, "The recipe of workspace %s is confirmed. Workers prepare its environment now.\n", cfg.Workspace)
			return nil
		},
	}
	cmd.AddCommand(draft, check, confirm)
	return cmd
}

func draftHere(ctx context.Context) (recipe.Draft, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return recipe.Draft{}, err
	}
	root, err := repo.Root(ctx, cwd)
	if err != nil {
		return recipe.Draft{}, err
	}
	files, err := repo.TrackedFiles(ctx, root, "HEAD")
	if err != nil {
		return recipe.Draft{}, err
	}
	return recipe.FromRepository(root, files), nil
}

func envContext(ctx context.Context) (string, repo.Config, *api.Client, error) {
	root, cfg, err := repoConfig(ctx)
	if err != nil {
		return "", cfg, nil, err
	}
	if cfg.Workspace == "" {
		return "", cfg, nil, cbx.Errorf(cbx.NoWorkspace, "casebox.yml names no workspace; run casebox init")
	}
	client, err := cliClient()
	return root, cfg, client, err
}

// recipeHash is the hash the server gives a recipe: SHA-256 of its canonical JSON.
func recipeHash(r repo.Recipe) string {
	return sha256Hex(r.JSON())
}

func envCheck(ctx context.Context, out io.Writer) error {
	root, cfg, client, err := envContext(ctx)
	if err != nil {
		return err
	}
	if cfg.Environment == nil {
		return errors.New("casebox.yml has no environment block; run casebox env draft and add it")
	}
	rec := *cfg.Environment
	if err := rec.Validate(); err != nil {
		return fmt.Errorf("casebox.yml environment: %w", err)
	}
	var ws workspaceView
	if err := client.Do(ctx, http.MethodPut, "/api/v1/workspaces/"+cfg.Workspace+"/recipe", map[string]json.RawMessage{"recipe": rec.JSON()}, &ws); err != nil {
		return fmt.Errorf("propose the recipe: %w", err)
	}

	provider, name, err := providers.Available(ctx)
	if err != nil {
		return err
	}
	commit, err := repo.Resolve(ctx, root, "HEAD")
	if err != nil {
		return err
	}
	files, err := repo.TrackedFiles(ctx, root, commit)
	if err != nil {
		return err
	}
	spec, err := recipe.Spec(rec, files, func(f string) ([]byte, error) { return repo.ReadAt(ctx, root, commit, f) })
	if err != nil {
		return err
	}
	tree, err := repo.Archive(ctx, root, commit)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Building the environment with %s (image %s, %d lockfiles)…\n", name, rec.Image, len(spec.Context))
	report, err := recipe.Check(ctx, provider, name, rec, spec, tree)
	if cerr := tree.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	report.Hash, report.Commit = ws.RecipeHash, commit

	if report.BuildError != "" {
		fmt.Fprintf(out, "The build failed after %.0fs:\n%s\n", report.BuildSeconds, report.BuildError)
	} else {
		fmt.Fprintf(out, "Built in %.0fs.\n", report.BuildSeconds)
	}
	for _, t := range report.Tests {
		fmt.Fprintf(out, "  %-9s %s (%.0fs)\n", t.Status, t.Command, t.Seconds)
		if t.Tail != "" {
			fmt.Fprintf(out, "%s\n", indent(t.Tail))
		}
	}

	data, _ := json.Marshal(report)
	ingest, err := ingestClient()
	if err != nil {
		return err
	}
	blob, err := ingest.PutBlob(ctx, "application/octet-stream", data)
	if err != nil {
		return fmt.Errorf("upload the report: %w", err)
	}
	if err := client.Do(ctx, http.MethodPost, "/api/v1/workspaces/"+cfg.Workspace+"/recipe/validation",
		map[string]any{"hash": ws.RecipeHash, "passed": report.Passed(), "reportBlob": blob}, &ws); err != nil {
		return fmt.Errorf("record the check: %w", err)
	}
	if report.Passed() {
		fmt.Fprintln(out, "The check passed. An admin confirms the recipe with casebox env confirm.")
		return nil
	}
	return errors.New("the check did not pass; fix the recipe in casebox.yml and run casebox env check again")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func indent(text string) string {
	return "    " + strings.ReplaceAll(text, "\n", "\n    ")
}
