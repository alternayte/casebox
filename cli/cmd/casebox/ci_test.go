package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const ciConfig = `version: 1
workspace: payments
harness:
  globs: ["AGENTS.md", ".claude/skills/**"]
evaluation:
  baseline: { agent: claude-code, agent_version: 2.1.284, model: claude-sonnet-5-20260801 }
prices:
  claude-sonnet-5-20260801: { input: 3, output: 15 }
suites:
  smoke: { size: 6, repeats: 1 }
`

// ciRepo is a checkout with casebox.yml, a base commit and a pull request's head commit that
// changes the given files; it returns the event file of that pull request.
func ciRepo(t *testing.T, changed map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("the test needs git")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write(".casebox/casebox.yml", ciConfig)
	write("AGENTS.md", "rules\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	for name, body := range changed {
		write(name, body)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "change")
	head := git("rev-parse", "HEAD")

	event := filepath.Join(t.TempDir(), "event.json")
	data, _ := json.Marshal(map[string]any{"pull_request": map[string]any{"number": 42, "head": map[string]string{"sha": head}, "base": map[string]string{"sha": base}}})
	if err := os.WriteFile(event, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return event
}

func ciEnv(t *testing.T, server string) {
	t.Setenv("CASEBOX_SERVER", server)
	t.Setenv("CASEBOX_TOKEN", "cbx_ci_test")
	t.Setenv("GITHUB_REPOSITORY", "Acme/Payments")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
}

// A pull request that changes a harness file asks for a smoke run with casebox.yml's baseline,
// prices and suite; the result is printed, and --fail-on regression makes a regression exit 1.
func TestCIRunsAPullRequestAndFailsOnARegression(t *testing.T) {
	var request map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cbx_ci_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/ci/pull-requests":
			_ = json.NewDecoder(r.Body).Decode(&request)
			_, _ = w.Write([]byte(`{"runs":[{"id":"run1","workspace":"payments","status":"started","evaluationId":"ev1"}]}`))
		case "GET /api/v1/ci/runs/run1":
			_, _ = w.Write([]byte(`{"id":"run1","kind":"pull_request","workspace":"payments","repo":"github.com/acme/payments","number":42,"headSha":"abc",
				"status":"done","evaluationId":"ev1","purpose":"harness_ci","repeats":1,"delta":0.05,"estimate":{"totalUsd":3,"detectableEffect":0.58},
				"spentUsd":2.5,"capUsd":150,"runsCompleted":2,"runsFailed":0,
				"verdict":{"verdict":"inconclusive","delta":-0.5,"lower":-1,"upper":0.2,"level":0.95,"cases":2,"runs":2,"baselineRate":1,"candidateRate":0.5,"reason":"smoke","regressions":["case-a"]},
				"cases":[{"caseId":"case-a","baselinePassed":3,"baselineRuns":3,"candidatePassed":0,"candidateRuns":1,"failedRuns":0,"regression":true},
				         {"caseId":"case-b","baselinePassed":3,"baselineRuns":3,"candidatePassed":1,"candidateRuns":1,"failedRuns":0,"regression":false}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	event := ciRepo(t, map[string]string{"AGENTS.md": "better rules\n", "main.go": "package main\n"})
	ciEnv(t, srv.URL)

	var out bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"ci", "--event", event, "--fail-on", "regression"})
	err := root.Execute()
	var exit exitError
	if !errors.As(err, &exit) || exit.code != 1 {
		t.Fatalf("exit %v, want code 1\n%s", err, out.String())
	}
	if request["repo"] != "github.com/acme/payments" || request["number"] != float64(42) || request["workspace"] != "payments" ||
		request["size"] != float64(6) || request["repeats"] != float64(1) || request["onSharedHarness"] != false {
		t.Fatalf("request %v", request)
	}
	spec := request["spec"].(map[string]any)
	if spec["harness"] != "HEAD" || spec["model"] != "claude-sonnet-5-20260801" {
		t.Fatalf("spec %v", spec)
	}
	text := out.String()
	for _, want := range []string{"Harness files changed: AGENTS.md", "1 regression(s)", "inconclusive (smoke run)", "case-a", "about 58 points"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output lacks %q:\n%s", want, text)
		}
	}
}

// A pull request that changes no harness file asks the server for nothing and passes.
func TestCISkipsAPullRequestWithoutAHarnessChange(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true }))
	defer srv.Close()
	event := ciRepo(t, map[string]string{"main.go": "package main\n"})
	ciEnv(t, srv.URL)

	var out bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetArgs([]string{"ci", "--event", event})
	if err := root.Execute(); err != nil || called || !strings.Contains(out.String(), "No harness file changed") {
		t.Fatalf("err %v, called %v:\n%s", err, called, out.String())
	}
}
