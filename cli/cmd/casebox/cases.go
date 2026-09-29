package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/harbor"
	"github.com/alternayte/casebox/cli/internal/recipe"
	"github.com/alternayte/casebox/cli/internal/repo"
)

// caseSummary is a row of GET /api/v1/cases.
type caseSummary struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	Workspace      string  `json:"workspace"`
	Status         string  `json:"status"`
	Scope          string  `json:"scope"`
	Source         string  `json:"source"`
	WorkItem       string  `json:"workItem"`
	FailToPass     *int    `json:"failToPass"`
	PassToPass     *int    `json:"passToPass"`
	Drift          bool    `json:"drift"`
	Weight         float64 `json:"weight"`
	Split          string  `json:"split"`
	HasInstruction bool    `json:"hasInstruction"`
}

type caseRepo struct {
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	Merged string `json:"merged"`
	Role   string `json:"role"`
}

// caseView is the answer of GET /api/v1/cases/{id} and each item of the review queue.
type caseView struct {
	Summary         caseSummary `json:"summary"`
	Repos           []caseRepo  `json:"repos"`
	Instruction     *string     `json:"instruction"`
	Signatures      []string    `json:"signatures"`
	Oracle          *string     `json:"oracle"`
	Seconds         *float64    `json:"seconds"`
	RecipeHash      string      `json:"recipeHash"`
	ApprovalBlocker *string     `json:"approvalBlocker"`
}

// caseOracle is the answer of GET /api/v1/cases/{id}/oracle.
type caseOracle struct {
	Tests struct {
		FailToPass []string `json:"failToPass"`
		PassToPass []string `json:"passToPass"`
	} `json:"tests"`
	TestFiles []string `json:"testFiles"`
}

// validationRun is one row of GET /api/v1/cases/{id}/validations.
type validationRun struct {
	Passed  bool     `json:"passed"`
	Seconds *float64 `json:"seconds"`
}

func casePath(id string, rest ...string) string {
	return "/api/v1/cases/" + url.PathEscape(id) + strings.Join(rest, "")
}

// listCases reads every case of GET /api/v1/cases with the query, a page at a time.
func listCases(ctx context.Context, client *api.Client, query url.Values) ([]caseSummary, error) {
	const page = 1000
	var all []caseSummary
	for offset := 0; ; offset += page {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("limit", fmt.Sprint(page))
		q.Set("offset", fmt.Sprint(offset))
		var rows []caseSummary
		if err := client.Do(ctx, http.MethodGet, "/api/v1/cases?"+q.Encode(), nil, &rows); err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < page {
			return all, nil
		}
	}
}

func newMineCommand() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "mine",
		Short: "Mine cases from the workspace's history (member)",
		Long: "Queue a mining job for each repository of the workspace in casebox.yml. A worker mines candidate cases\n" +
			"from its mirror. A worker with a sandbox provider validates them. A worker with an analysis model drafts\n" +
			"their instructions. With --wait, it polls until the case counts stop changing (at most 30 minutes).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			_, cfg, client, err := envContext(ctx)
			if err != nil {
				return err
			}
			var res struct {
				Jobs int `json:"jobs"`
			}
			if err := client.Do(ctx, http.MethodPost, "/api/v1/workspaces/"+url.PathEscape(cfg.Workspace)+"/mining", nil, &res); err != nil {
				return fmt.Errorf("start mining: %w", err)
			}
			fmt.Fprintf(out, "Mining queued for %d repositories of workspace %s.\n", res.Jobs, cfg.Workspace)
			fmt.Fprintln(out, "A worker with a sandbox provider validates the cases it mines (casebox worker; CASEBOX_SANDBOX, default docker).")
			if !wait {
				fmt.Fprintln(out, "Review them with casebox review once they are validated.")
				return nil
			}
			return waitForCases(ctx, out, client, cfg.Workspace, 10*time.Second, time.Minute, 30*time.Minute)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "poll until the case counts stop changing (at most 30 minutes)")
	return cmd
}

// waitForCases polls the workspace's cases every interval and returns once the counts by status
// have not changed for stable, or after limit.
func waitForCases(ctx context.Context, out io.Writer, client *api.Client, workspace string, interval, stable, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	var last string
	changed := time.Now()
	for {
		rows, err := listCases(ctx, client, url.Values{"workspace": {workspace}})
		if err != nil {
			return err
		}
		line := statusCounts(rows)
		if line != last {
			fmt.Fprintf(out, "Cases: %s\n", line)
			last, changed = line, time.Now()
		}
		if time.Since(changed) >= stable {
			fmt.Fprintln(out, "The counts stopped changing. Review the validated cases with casebox review.")
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(out, "Stopped waiting after %s; the workers go on. Check again with casebox mine --wait or in the UI.\n", limit)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// statusCounts is "N in all: n status, …" in the order a case moves through.
func statusCounts(rows []caseSummary) string {
	order := []string{"mined", "validated", "validation_failed", "approved", "rejected", "retired"}
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Status]++
	}
	for status := range counts {
		known := false
		for _, o := range order {
			known = known || o == status
		}
		if !known {
			order = append(order, status)
		}
	}
	parts := []string{}
	for _, status := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[status], strings.ReplaceAll(status, "_", " ")))
	}
	return fmt.Sprintf("%d in all: %s", len(rows), strings.Join(parts, ", "))
}

func newReviewCommand() *cobra.Command {
	var approveAll bool
	var workspace string
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Review validated cases: approve, edit, reject or skip (member)",
		Long: "Walk the review queue (validated cases with a drafted instruction). For each case it prints the source,\n" +
			"the tests that decide it, the instruction and the interfaces. Then it asks: [a]pprove, [e]dit the instruction\n" +
			"in $EDITOR, [r]eject with a reason, [s]kip or [q]uit. --approve-all approves the whole queue and lists the\n" +
			"cases the server refused, with the reason.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := cliClient()
			if err != nil {
				return err
			}
			r := reviewer{client: client, in: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout(), edit: editText}
			if approveAll {
				return r.approveAll(cmd.Context(), workspace)
			}
			return r.walk(cmd.Context(), workspace)
		},
	}
	cmd.Flags().BoolVar(&approveAll, "approve-all", false, "approve every case in the queue")
	cmd.Flags().StringVar(&workspace, "workspace", "", "review only this workspace's cases (default: all)")
	return cmd
}

// reviewer walks the review queue. edit opens the text in an editor and returns what was saved.
type reviewer struct {
	client *api.Client
	in     *bufio.Reader
	out    io.Writer
	edit   func(text string) (string, error)
}

func (r reviewer) queue(ctx context.Context, workspace string) ([]caseView, error) {
	path := "/api/v1/cases/queue"
	if workspace != "" {
		path += "?workspace=" + url.QueryEscape(workspace)
	}
	var queue []caseView
	if err := r.client.Do(ctx, http.MethodGet, path, nil, &queue); err != nil {
		return nil, fmt.Errorf("read the review queue: %w", err)
	}
	return queue, nil
}

func (r reviewer) approveAll(ctx context.Context, workspace string) error {
	queue, err := r.queue(ctx, workspace)
	if err != nil {
		return err
	}
	if len(queue) == 0 {
		fmt.Fprintln(r.out, "The review queue is empty.")
		return nil
	}
	ids := make([]string, len(queue))
	for i, c := range queue {
		ids[i] = c.Summary.ID
	}
	var res struct {
		Approved []string `json:"approved"`
		Refused  []struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"refused"`
	}
	if err := r.client.Do(ctx, http.MethodPost, "/api/v1/cases/approvals", map[string][]string{"ids": ids}, &res); err != nil {
		return fmt.Errorf("approve the queue: %w", err)
	}
	fmt.Fprintf(r.out, "Approved %d of %d cases.\n", len(res.Approved), len(ids))
	if len(res.Refused) > 0 {
		fmt.Fprintf(r.out, "Refused %d:\n", len(res.Refused))
		for _, f := range res.Refused {
			fmt.Fprintf(r.out, "  %s: %s\n", f.ID, f.Reason)
		}
	}
	return nil
}

func (r reviewer) walk(ctx context.Context, workspace string) error {
	queue, err := r.queue(ctx, workspace)
	if err != nil {
		return err
	}
	if len(queue) == 0 {
		fmt.Fprintln(r.out, "The review queue is empty.")
		return nil
	}
	approved, rejected, skipped := 0, 0, 0
	defer func() {
		fmt.Fprintf(r.out, "Approved %d, rejected %d, skipped %d.\n", approved, rejected, skipped)
	}()
	for i := 0; i < len(queue); i++ {
		c := queue[i]
		fmt.Fprintf(r.out, "\nCase %d of %d\n", i+1, len(queue))
		r.print(ctx, c)
		for decided := false; !decided; {
			options := "[a]pprove, [e]dit, [r]eject, [s]kip, [q]uit"
			if c.ApprovalBlocker != nil {
				fmt.Fprintf(r.out, "Approval is not possible: %s\n", *c.ApprovalBlocker)
				options = "[e]dit, [r]eject, [s]kip, [q]uit"
			}
			answer, ok := r.ask(options + "? ")
			if !ok {
				return nil
			}
			switch strings.ToLower(answer) {
			case "a", "approve":
				if c.ApprovalBlocker != nil {
					continue
				}
				if err := r.client.Do(ctx, http.MethodPost, casePath(c.Summary.ID, "/approval"), nil, nil); err != nil {
					fmt.Fprintf(r.out, "The server refused the approval: %v\n", err)
					continue
				}
				fmt.Fprintln(r.out, "Approved.")
				approved++
				decided = true
			case "e", "edit":
				current := ""
				if c.Instruction != nil {
					current = *c.Instruction
				}
				text, err := r.edit(current)
				if err != nil {
					fmt.Fprintf(r.out, "The editor failed: %v\n", err)
					continue
				}
				text = strings.TrimSpace(text)
				if text == "" || text == strings.TrimSpace(current) {
					fmt.Fprintln(r.out, "The instruction is unchanged.")
					continue
				}
				if err := r.client.Do(ctx, http.MethodPut, casePath(c.Summary.ID, "/instruction"), map[string]string{"text": text}, nil); err != nil {
					fmt.Fprintf(r.out, "The server refused the edit: %v\n", err)
					continue
				}
				var fresh caseView
				if err := r.client.Do(ctx, http.MethodGet, casePath(c.Summary.ID), nil, &fresh); err != nil {
					return err
				}
				c = fresh
				fmt.Fprintln(r.out, "Saved. The instruction now reads:")
				fmt.Fprintln(r.out, indent(instructionText(c)))
			case "r", "reject":
				reason, ok := r.ask("Reason (empty to go back): ")
				if !ok {
					return nil
				}
				if reason == "" {
					continue
				}
				if err := r.client.Do(ctx, http.MethodPost, casePath(c.Summary.ID, "/rejection"), map[string]string{"reason": reason}, nil); err != nil {
					fmt.Fprintf(r.out, "The server refused the rejection: %v\n", err)
					continue
				}
				fmt.Fprintln(r.out, "Rejected.")
				rejected++
				decided = true
			case "s", "skip":
				skipped++
				decided = true
			case "q", "quit":
				return nil
			default:
				fmt.Fprintln(r.out, "Answer a, e, r, s or q.")
			}
		}
	}
	return nil
}

// ask prints the prompt and reads one line; false at the end of the input.
func (r reviewer) ask(prompt string) (string, bool) {
	fmt.Fprint(r.out, prompt)
	line, err := r.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(r.out)
		return "", false
	}
	return strings.TrimSpace(line), true
}

func (r reviewer) print(ctx context.Context, c caseView) {
	s := c.Summary
	fmt.Fprintf(r.out, "%s: %s case, %s scope, workspace %s\n", s.ID, s.Kind, s.Scope, s.Workspace)
	fmt.Fprintf(r.out, "Source: %s\n", s.Source)
	if s.WorkItem != "" {
		fmt.Fprintf(r.out, "Work item: %s\n", s.WorkItem)
	} else {
		fmt.Fprintln(r.out, "Work item: none. The instruction comes from the pull request title only.")
	}
	fmt.Fprintf(r.out, "Tests: %s fail-to-pass, %s pass-to-pass\n", count(s.FailToPass), count(s.PassToPass))
	drift := "no drift"
	if s.Drift {
		drift = "the harness drifts from this base"
	}
	fmt.Fprintf(r.out, "Weight: %g (%s)\n", s.Weight, drift)
	fmt.Fprintln(r.out, "Instruction:")
	fmt.Fprintln(r.out, indent(instructionText(c)))
	if len(c.Signatures) > 0 {
		fmt.Fprintln(r.out, "Interfaces your solution must provide:")
		fmt.Fprintln(r.out, indent(strings.Join(c.Signatures, "\n")))
	}
	var o caseOracle
	if err := r.client.Do(ctx, http.MethodGet, casePath(s.ID, "/oracle"), nil, &o); err != nil {
		fmt.Fprintf(r.out, "Fail-to-pass tests: the oracle is not readable (%v)\n", err)
		return
	}
	fmt.Fprintln(r.out, "Fail-to-pass tests:")
	for _, id := range o.Tests.FailToPass {
		fmt.Fprintf(r.out, "    %s\n", id)
	}
}

func instructionText(c caseView) string {
	if c.Instruction == nil {
		return "(none: the instruction was erased; edit it to write a new one)"
	}
	return strings.TrimSpace(*c.Instruction)
}

func count(n *int) string {
	if n == nil {
		return "?"
	}
	return fmt.Sprint(*n)
}

// editText opens text in $EDITOR (vi, or notepad on Windows, without it) and returns the saved file.
func editText(text string) (string, error) {
	f, err := os.CreateTemp("", "casebox-instruction-*.md")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(text + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	editor := strings.Fields(os.Getenv("EDITOR"))
	if len(editor) == 0 {
		editor = []string{"vi"}
		if runtime.GOOS == "windows" {
			editor = []string{"notepad"}
		}
	}
	cmd := exec.Command(editor[0], append(editor[1:], f.Name())...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(editor, " "), err)
	}
	data, err := os.ReadFile(f.Name())
	return string(data), err
}

func newCasesCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "cases", Short: "Approved cases"}
	var format, out, workspace string
	export := &cobra.Command{
		Use:   "export --format harbor --out <dir> [ids…]",
		Short: "Export approved cases as Harbor tasks",
		Long: "Write each approved case (all of the workspace's, or the ids given) as a Harbor task (task format 1.4)\n" +
			"in <dir>/<workspace>-<case id>. Run it inside a checkout of the case's repository. The base tree comes\n" +
			"from git archive of the base commit, fetched from origin when missing. The recipe comes from casebox.yml,\n" +
			"and must be the recipe the case was validated with. Cases that span other repositories and steering\n" +
			"cases (decided by trace assertions) are skipped.\n" +
			"The oracle and the patches are blobs only a worker token may read: set CASEBOX_WORKER_TOKEN.",
		RunE: func(cmd *cobra.Command, ids []string) error {
			if format != "harbor" {
				return fmt.Errorf("format %q: the only export format is harbor", format)
			}
			if out == "" {
				return errors.New("name the output directory with --out")
			}
			return exportCases(cmd.Context(), cmd.OutOrStdout(), out, workspace, ids)
		},
	}
	export.Flags().StringVar(&format, "format", "harbor", "the task format: harbor")
	export.Flags().StringVar(&out, "out", "", "the directory the tasks go in")
	export.Flags().StringVar(&workspace, "workspace", "", "the workspace (default: casebox.yml's)")
	cmd.AddCommand(export)
	return cmd
}

func exportCases(ctx context.Context, out io.Writer, dir, workspace string, ids []string) error {
	token := os.Getenv("CASEBOX_WORKER_TOKEN")
	if token == "" {
		return errors.New("the oracle and patch blobs need a worker token: set CASEBOX_WORKER_TOKEN (Settings → Tokens)")
	}
	root, cfg, err := repoConfig(ctx)
	if err != nil {
		return err
	}
	if workspace == "" {
		workspace = cfg.Workspace
	}
	if workspace == "" && len(ids) == 0 {
		return errors.New("name the workspace with --workspace or in casebox.yml")
	}
	if cfg.Environment == nil {
		return errors.New("casebox.yml has no environment block: the export builds the recipe the cases were validated with")
	}
	rec := *cfg.Environment
	here := repo.Current(ctx, root).Repo
	if here == "" {
		return errors.New("this checkout has no origin remote, so its cases cannot be matched to it")
	}
	client, err := cliClient()
	if err != nil {
		return err
	}
	blobs := api.New(client.Server, token)
	blobs.HTTP = &http.Client{Timeout: 5 * time.Minute}

	var views []caseView
	if len(ids) == 0 {
		rows, err := listCases(ctx, client, url.Values{"status": {"approved"}, "workspace": {workspace}})
		if err != nil {
			return err
		}
		ids = make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
	}
	for _, id := range ids {
		var v caseView
		if err := client.Do(ctx, http.MethodGet, casePath(id), nil, &v); err != nil {
			return fmt.Errorf("read case %s: %w", id, err)
		}
		views = append(views, v)
	}
	if len(views) == 0 {
		fmt.Fprintf(out, "Workspace %s has no approved cases.\n", workspace)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	exported := 0
	for _, v := range views {
		why, err := exportCase(ctx, client, blobs, root, here, rec, dir, v)
		if err != nil {
			return fmt.Errorf("case %s: %w", v.Summary.ID, err)
		}
		if why != "" {
			fmt.Fprintf(out, "Skipped %s: %s\n", v.Summary.ID, why)
			continue
		}
		exported++
		fmt.Fprintf(out, "Wrote %s\n", filepath.Join(dir, v.Summary.Workspace+"-"+v.Summary.ID))
	}
	fmt.Fprintf(out, "Exported %d of %d cases.\n", exported, len(views))
	return nil
}

// exportCase writes one case as a Harbor task, or returns why it does not export.
func exportCase(ctx context.Context, client, blobs *api.Client, root, here string, rec repo.Recipe, dir string, v caseView) (string, error) {
	s := v.Summary
	switch {
	case s.Status != "approved":
		return "only approved cases export; this one is " + strings.ReplaceAll(s.Status, "_", " "), nil
	case v.Instruction == nil || strings.TrimSpace(*v.Instruction) == "":
		return "its instruction was erased; it needs a new one before it exports", nil
	case s.Kind == "steering":
		return "a steering case is decided by trace assertions and judge questions, which a Harbor verifier cannot check", nil
	case v.Oracle == nil:
		return "it has no oracle", nil
	case v.RecipeHash != recipeHash(rec):
		return "the recipe in casebox.yml is not the one the case was validated with", nil
	}
	var sealed []caseRepo
	var others []string
	for _, r := range v.Repos {
		if r.Repo != here {
			others = append(others, r.Repo)
		}
		if r.Role == "sealed" {
			sealed = append(sealed, r)
		}
	}
	if len(others) > 0 {
		return fmt.Sprintf("it spans repositories other than this checkout (%s); run the export in a checkout of each", strings.Join(others, ", ")), nil
	}
	if len(sealed) != 1 {
		return fmt.Sprintf("it seals %d repositories; a Harbor task holds one", len(sealed)), nil
	}
	base := sealed[0].Base

	var runs []validationRun
	if err := client.Do(ctx, http.MethodGet, casePath(s.ID, "/validations"), nil, &runs); err != nil {
		return "", fmt.Errorf("read the validation runs: %w", err)
	}
	seconds := 0.0
	if v.Seconds != nil {
		seconds = *v.Seconds
	}
	for _, r := range runs {
		if r.Passed && r.Seconds != nil {
			seconds = math.Max(seconds, *r.Seconds)
		}
	}

	oracleBlob, err := getBlob(ctx, blobs, *v.Oracle)
	if err != nil {
		return "", fmt.Errorf("read the oracle: %w", err)
	}
	var o harbor.Oracle
	if err := json.Unmarshal(oracleBlob, &o); err != nil {
		return "", fmt.Errorf("read the oracle: %w", err)
	}
	if o.SourcePatch == "" || o.TestPatch == "" {
		return "its oracle names no source or test patch", nil
	}
	sourcePatch, err := getBlob(ctx, blobs, o.SourcePatch)
	if err != nil {
		return "", fmt.Errorf("read the source patch: %w", err)
	}
	testPatch, err := getBlob(ctx, blobs, o.TestPatch)
	if err != nil {
		return "", fmt.Errorf("read the test patch: %w", err)
	}

	if err := ensureCommit(ctx, root, base); err != nil {
		return "", err
	}
	files, err := repo.TrackedFiles(ctx, root, base)
	if err != nil {
		return "", err
	}
	env, err := recipe.Spec(rec, files, func(f string) ([]byte, error) { return repo.ReadAt(ctx, root, base, f) })
	if err != nil {
		return "", err
	}
	tree, err := repo.Archive(ctx, root, base)
	if err != nil {
		return "", err
	}
	task := harbor.Task{
		Case: harbor.Case{
			ID: s.ID, Workspace: s.Workspace, Kind: s.Kind, Scope: s.Scope, Source: s.Source, WorkItem: s.WorkItem,
			FailToPass: len(o.Tests.FailToPass), PassToPass: len(o.Tests.PassToPass), Drift: s.Drift, Weight: s.Weight,
			Seconds: seconds, Instruction: *v.Instruction, Signatures: v.Signatures,
		},
		Oracle: o, Recipe: rec, Env: env, Base: tree, SourcePatch: sourcePatch, TestPatch: testPatch,
	}
	err = harbor.Write(filepath.Join(dir, task.Name()), task)
	if cerr := tree.Close(); err == nil && cerr != nil {
		err = cerr
	}
	return "", err
}

// getBlob reads a blob with the worker token and checks it is the content its hash names.
func getBlob(ctx context.Context, client *api.Client, hash string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.Server+"/worker/v1/blobs/"+url.PathEscape(hash), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+client.Token)
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", client.Server, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("the server answered %d: CASEBOX_WORKER_TOKEN must be a worker token of this organisation", resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &api.Error{Status: resp.StatusCode}
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("blob %s: the content does not match its hash", hash)
	}
	return data, nil
}

// ensureCommit fetches the commit from origin when the checkout does not have it.
func ensureCommit(ctx context.Context, root, sha string) error {
	if exec.CommandContext(ctx, "git", "-C", root, "cat-file", "-e", sha+"^{commit}").Run() == nil {
		return nil
	}
	fetch := exec.CommandContext(ctx, "git", "-C", root, "fetch", "--quiet", "--no-tags", "origin", sha)
	if out, err := fetch.CombinedOutput(); err != nil {
		return fmt.Errorf("the base commit %s is not in this checkout and git fetch origin %s failed: %v: %s", sha, sha, err, strings.TrimSpace(string(out)))
	}
	return nil
}
