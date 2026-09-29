package evaluate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/cases"
	"github.com/alternayte/casebox/cli/internal/oracle"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/runner"
)

func f(v float64) *float64 { return &v }

func TestCost(t *testing.T) {
	u := capture.Usage{InputTokens: 1_000_000, OutputTokens: 200_000, CacheReadTokens: 4_000_000, CacheWriteTokens: 500_000}
	cases := []struct {
		name  string
		usage capture.Usage
		price repo.Price
		want  float64
	}{
		// 3 + 0.2×15 + 4×0.3 + 0.5×3.75
		{"every price", u, repo.Price{Input: f(3), Output: f(15), CacheRead: f(0.3), CacheWrite: f(3.75)}, 3 + 3 + 1.2 + 1.875},
		// Cache reads and writes without a price cost as input: 3 + 3 + 4×3 + 0.5×3.
		{"cache priced as input", u, repo.Price{Input: f(3), Output: f(15)}, 3 + 3 + 12 + 1.5},
		{"the agent's own figure wins", capture.Usage{InputTokens: 1_000_000, CostUSD: f(0.42)}, repo.Price{Input: f(3), Output: f(15)}, 0.42},
		{"no tokens", capture.Usage{}, repo.Price{Input: f(3), Output: f(15)}, 0},
	}
	for _, c := range cases {
		if got := Cost(c.usage, c.price); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: cost %v, want %v", c.name, got, c.want)
		}
	}
}

const cannedDiff = `diff --git a/migrations/001.sql b/migrations/001.sql
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ b/migrations/001.sql
@@ -0,0 +1 @@
+CREATE TABLE t (id int);
diff --git a/store/store.go b/store/store.go
index 2222222..3333333 100644
--- a/store/store.go
+++ b/store/store.go
@@ -1,3 +1,4 @@
 package store
-import "gomock"
+
+func Get() int { return 2 }
`

func TestAssertions(t *testing.T) {
	tool := func(command string) capture.Event {
		return capture.Event{Kind: capture.KindToolCall, Tool: &capture.Tool{Name: "Bash"}, Attrs: map[string]string{"command": command}}
	}
	response := capture.Event{Kind: capture.KindResponse, Text: "done"}
	testedFirst := []capture.Event{tool("cd store && go test ./..."), response}
	testedAfter := []capture.Event{response, tool("go test ./...")}
	neverDone := []capture.Event{tool("go test ./...")}

	check := func(a cases.Assertion, diff string, events []capture.Event) bool {
		t.Helper()
		got := CheckAssertions([]cases.Assertion{a}, []byte(diff), events)
		if len(got) != 1 || got[0].Kind != a.Kind {
			t.Fatalf("CheckAssertions = %+v", got)
		}
		return got[0].Passed
	}
	for _, c := range []struct {
		name   string
		a      cases.Assertion
		diff   string
		events []capture.Event
		want   bool
	}{
		{"a forbidden file touched", cases.Assertion{Kind: ForbiddenFile, Path: "migrations/**"}, cannedDiff, nil, false},
		{"no forbidden file touched", cases.Assertion{Kind: ForbiddenFile, Path: "web/**"}, cannedDiff, nil, true},
		{"the tests ran before done", cases.Assertion{Kind: CommandBeforeDone, Pattern: `go test`}, "", testedFirst, true},
		{"the tests ran only after done", cases.Assertion{Kind: CommandBeforeDone, Pattern: `go test`}, "", testedAfter, false},
		{"the agent never said done", cases.Assertion{Kind: CommandBeforeDone, Pattern: `go test`}, "", neverDone, false},
		{"an added line matches", cases.Assertion{Kind: DiffMustMatch, Pattern: `func Get\(\) int`}, cannedDiff, nil, true},
		// "gomock" is only on a removed line, and "store.go" only in the headers.
		{"a removed line does not count", cases.Assertion{Kind: DiffMustNotMatch, Pattern: `gomock`}, cannedDiff, nil, true},
		{"a header does not count", cases.Assertion{Kind: DiffMustMatch, Pattern: `store\.go`}, cannedDiff, nil, false},
		{"an added line must not match", cases.Assertion{Kind: DiffMustNotMatch, Pattern: `(?m)^CREATE TABLE`}, cannedDiff, nil, false},
		{"a pattern that does not compile", cases.Assertion{Kind: DiffMustMatch, Pattern: `(`}, cannedDiff, nil, false},
		{"an unknown kind", cases.Assertion{Kind: "other", Pattern: `x`}, cannedDiff, nil, false},
	} {
		if got := check(c.a, c.diff, c.events); got != c.want {
			t.Errorf("%s: passed %v, want %v", c.name, got, c.want)
		}
	}
}

// A test the oracle names that has no result counts as failed, and a command without a results
// file counts by its exit code.
func TestCountTests(t *testing.T) {
	o := cases.Oracle{Tests: cases.OracleTests{FailToPass: []string{"p::TestNew", "p::TestGone"}, PassToPass: []string{"p::TestOld"}},
		Commands: []cases.OracleCommand{{Command: "go test -json ./... > /results/go.json", Results: oracle.GoTestJSON}, {Command: "go vet ./..."}}}
	goJSON := `{"Action":"pass","Package":"p","Test":"TestNew","Elapsed":0.1}
{"Action":"pass","Package":"p","Test":"TestOld","Elapsed":0.1}
{"Action":"pass","Package":"p","Elapsed":0.2}
`
	res := runner.Result{Applied: true, Commands: []runner.CommandResult{
		{Command: o.Commands[0].Command, Results: oracle.GoTestJSON, Files: map[string][]byte{"go.json": []byte(goJSON)}},
		{Command: "go vet ./...", ExitCode: 1},
	}}
	counts, failed, err := Count(o, res)
	if err != nil {
		t.Fatal(err)
	}
	want := TestCounts{FailToPass: TestCount{Passed: 1, Total: 2}, PassToPass: TestCount{Passed: 1, Total: 2}}
	if counts != want || !reflect.DeepEqual(failed, []string{"p::TestGone", "exit:go vet ./..."}) {
		t.Fatalf("counts %+v, failed %v", counts, failed)
	}

	counts, failed, err = Count(o, runner.Result{FailedPatch: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	want = TestCounts{FailToPass: TestCount{Passed: 0, Total: 2}, PassToPass: TestCount{Passed: 0, Total: 2}}
	if counts != want || len(failed) != 4 {
		t.Fatalf("not applied: counts %+v, failed %v", counts, failed)
	}
}

// gitRepo is a repository on disk for tests; it stands in for a mirror.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("the test needs git")
	}
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files (an empty body removes the file) and commits them; it returns the commit.
func (r *gitRepo) commit(msg string, files map[string]string) string {
	r.t.Helper()
	for name, body := range files {
		p := filepath.Join(r.dir, filepath.FromSlash(name))
		if body == "" {
			r.git("rm", "-q", "--", name)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// The harness of a side: none removes the files of the default branch's globs; a ref overlays
// the files its own casebox.yml globs match at that commit, and the hash is repo.Harness's.
func TestSelectHarness(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base := r.commit("base", map[string]string{"AGENTS.md": "old\n", "main.go": "package main\n"})
	r.git("checkout", "-q", "-b", "candidate")
	r.commit("candidate harness", map[string]string{
		".casebox/casebox.yml": "version: 1\nharness:\n  globs: [\"AGENTS.md\", \"docs/agent/**\"]\n",
		"AGENTS.md":            "new\n", "docs/agent/style.md": "style\n", "CLAUDE.md": "not a glob here\n",
	})
	r.git("checkout", "-q", "main")
	r.commit("main harness", map[string]string{"CLAUDE.md": "main\n", ".claude/skills/x/SKILL.md": "skill\n"})
	sealed := []sealedRepo{{CaseRepo: cases.CaseRepo{Repo: "example.com/r", Base: base, Role: cases.RoleSealed}, dir: r.dir}}

	h, err := selectHarness(ctx, sealed, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.globs, []string{"AGENTS.md", "docs/agent/**"}) || h.none {
		t.Fatalf("globs %v, none %v", h.globs, h.none)
	}
	if !reflect.DeepEqual(h.files, map[string][]byte{"AGENTS.md": []byte("new\n"), "docs/agent/style.md": []byte("style\n")}) {
		t.Fatalf("files %q", h.files)
	}
	want, err := repo.Harness(ctx, r.dir, "candidate", h.globs)
	if err != nil || h.hash != want.Hash || len(h.hash) != 64 {
		t.Fatalf("hash %s, want %s (%v)", h.hash, want.Hash, err)
	}

	h, err = selectHarness(ctx, sealed, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.globs, repo.DefaultHarnessGlobs) || len(h.files) != 3 {
		t.Fatalf("main: globs %v, files %q", h.globs, h.files)
	}

	h, err = selectHarness(ctx, sealed, "none")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(nil)
	if !h.none || len(h.files) != 0 || !reflect.DeepEqual(h.globs, repo.DefaultHarnessGlobs) || h.hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("none: %+v", h)
	}

	// Two sealed repositories: each under its folder, and one hash over both.
	two := []sealedRepo{sealed[0], sealed[0]}
	two[0].prefix, two[1].prefix = "a/", "b/"
	h, err = selectHarness(ctx, two, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.files["b/docs/agent/style.md"]; !ok || !reflect.DeepEqual(h.globs[:2], []string{"a/AGENTS.md", "a/docs/agent/**"}) || len(h.hash) != 64 || h.hash == want.Hash {
		t.Fatalf("two repositories: %+v", h)
	}

	if _, err := selectHarness(ctx, sealed, "no-such-branch"); err == nil {
		t.Fatal("a ref that names no commit was accepted")
	}
}
