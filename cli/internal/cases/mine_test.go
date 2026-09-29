package cases

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/repo"
)

// gitRepo is a throwaway repository on main.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *gitRepo {
	t.Helper()
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=dev@example.com", "-c", "user.name=Dev", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files (an empty body deletes the file) and commits them; it returns the commit.
func (r *gitRepo) commit(msg string, files map[string]string) string {
	r.t.Helper()
	for name, body := range files {
		p := filepath.Join(r.dir, filepath.FromSlash(name))
		if body == "" {
			if err := os.Remove(p); err != nil {
				r.t.Fatal(err)
			}
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
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// pull makes a branch off main with files, merges it with a merge commit, and returns the merge.
func (r *gitRepo) pull(branch string, files map[string]string) string {
	r.t.Helper()
	r.git("checkout", "-q", "-b", branch, "main")
	r.commit(branch, files)
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--no-ff", "-m", "Merge "+branch, branch)
	return r.git("rev-parse", "HEAD")
}

func opener(repos map[string]*gitRepo) source {
	return func(_ context.Context, name string) (string, error) {
		r, ok := repos[name]
		if !ok {
			return "", os.ErrNotExist
		}
		return r.dir, nil
	}
}

const apiRepo = "github.com/acme/api"

func TestMineFiltersAndScopes(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	recent := now.Add(-24 * time.Hour)

	r := newRepo(t)
	r.commit("start", map[string]string{
		".casebox/casebox.yml": "version: 1\ncases:\n  max_source_files: 2\n",
		"AGENTS.md":            "Run go test ./...\n",
		"store/store.go":       "package store\n",
		"store/store_test.go":  "package store\n",
		"web/app.ts":           "export const a = 1\n",
	})
	capability := r.pull("capability", map[string]string{
		"store/store.go":      "package store\n\nfunc Get() int { return 1 }\n",
		"store/store_test.go": "package store\n\n// TestGet\n",
	})
	sourceOnly := r.pull("source-only", map[string]string{"store/store.go": "package store\n\nfunc Get() int { return 2 }\n"})
	testsOnly := r.pull("tests-only", map[string]string{"store/more_test.go": "package store\n"})
	tooMany := r.pull("too-many", map[string]string{"a.go": "package a\n", "b.go": "package a\n", "c.go": "package a\n", "a_test.go": "package a\n"})
	// A squash merge: one commit on main, whose parent is its base.
	squash := r.commit("squash", map[string]string{"web/app.ts": "export const a = 2\n", "web/__tests__/app.ts": "test\n"})
	fix := r.pull("fix", map[string]string{"store/store.go": "package store\n\nfunc Get() int { return 3 }\n", "store/get_test.go": "package store\n"})
	fixNoTests := r.pull("fix-no-tests", map[string]string{"store/store.go": "package store\n\nfunc Get() int { return 4 }\n"})
	r.git("checkout", "-q", "-b", "unmerged", "main")
	unmerged := r.commit("unmerged", map[string]string{"x.go": "package x\n", "x_test.go": "package x\n"})
	r.git("checkout", "-q", "main")

	web := newRepo(t)
	web.commit("start", map[string]string{"index.ts": "1\n"})
	webMerge := web.pull("web-part", map[string]string{"index.ts": "2\n"})

	pull := func(n int, merge string, mod ...func(*Pull)) Pull {
		p := Pull{Repo: apiRepo, Number: n, BaseRef: "main", BaseSha: "0000", MergeSha: merge, MergedAt: recent}
		for _, m := range mod {
			m(&p)
		}
		return p
	}
	cand := func(key, kind string, pulls ...Pull) Candidate {
		return Candidate{Key: key, Kind: kind, Rank: 10, Pulls: pulls, Related: []Pull{}}
	}
	withRelated := cand("pr:split", Capability, pull(1, capability))
	withRelated.Related = []Pull{{Repo: "github.com/acme/web", Number: 3, BaseRef: "main", MergeSha: webMerge, MergedAt: recent}}
	regression := cand("fix:ok", Regression, pull(20, fix))
	// The original may be older than the window.
	regression.Original = ptr(pull(1, capability, func(p *Pull) { p.MergedAt = now.AddDate(0, 0, -400) }))
	regressionNoTests := cand("fix:no-tests", Regression, pull(21, fixNoTests))
	regressionNoTests.Original = ptr(pull(1, capability))
	steering := cand("steering:ok", Steering, pull(1, capability))
	steering.Base = ptr(sourceOnly)
	steeringUnknown := cand("steering:gone", Steering)
	steeringUnknown.Base = ptr(strings.Repeat("ab", 20))

	payload := MinePayload{Workspace: "payments", Repo: apiRepo, WindowDays: 183, MaxSourceFiles: 12, Candidates: []Candidate{
		cand("pr:capability", Capability, pull(1, capability)),
		cand("pr:source-only", Capability, pull(2, sourceOnly)),
		cand("pr:tests-only", Capability, pull(3, testsOnly)),
		cand("pr:too-many", Capability, pull(4, tooMany)),
		cand("pr:squash", Capability, pull(5, squash)),
		cand("pr:bot", Capability, pull(6, capability, func(p *Pull) { p.Bot = true })),
		cand("pr:develop", Capability, pull(7, capability, func(p *Pull) { p.BaseRef = "develop" })),
		cand("pr:old", Capability, pull(8, capability, func(p *Pull) { p.MergedAt = now.AddDate(0, 0, -400) })),
		cand("pr:missing", Capability, pull(9, strings.Repeat("cd", 20))),
		cand("pr:unmerged", Capability, pull(10, unmerged)),
		withRelated,
		regression,
		regressionNoTests,
		steering,
		steeringUnknown,
	}}
	res, err := mine(ctx, opener(map[string]*gitRepo{apiRepo: r, "github.com/acme/web": web}), payload, now)
	if err != nil {
		t.Fatal(err)
	}

	skipped := map[string]string{}
	for _, s := range res.Skipped {
		skipped[s.Key] = s.Reason
	}
	for key, want := range map[string]string{
		"pr:source-only": SkipNoTestChange,
		"pr:tests-only":  SkipNoSourceChange,
		"pr:too-many":    SkipTooManySources,
		"pr:bot":         SkipBot,
		"pr:develop":     SkipNotDefaultBranch,
		"pr:old":         SkipOutsideWindow,
		"pr:missing":     SkipNotMerged,
		"pr:unmerged":    SkipNotMerged,
		"fix:no-tests":   SkipNoTestChange,
		"steering:gone":  SkipBaseUnknown,
	} {
		if skipped[key] != want {
			t.Errorf("%s: skipped as %q, want %q", key, skipped[key], want)
		}
	}

	mined := map[string]MinedCase{}
	for _, c := range res.Cases {
		mined[c.Key] = c
	}
	if len(mined)+len(skipped) != len(payload.Candidates) {
		t.Fatalf("%d cases and %d skipped for %d candidates", len(mined), len(skipped), len(payload.Candidates))
	}
	parent := func(sha string) string { return r.git("rev-parse", sha+"^1") }

	c := mined["pr:capability"]
	if c.Scope != ScopeSingle || len(c.Repos) != 1 || c.Repos[0].Base != parent(capability) || *c.Repos[0].Merged != capability || c.Repos[0].Role != RoleSealed {
		t.Errorf("capability: %+v", c)
	}
	if c.TestFiles != 1 || c.SourceFiles != 1 || c.Rank != 10 {
		t.Errorf("capability counts: %d test, %d source files, rank %d", c.TestFiles, c.SourceFiles, c.Rank)
	}
	h, err := repo.Harness(ctx, r.dir, parent(capability), repo.DefaultHarnessGlobs)
	if err != nil || c.HarnessHash != h.Hash {
		t.Errorf("harness hash %q, want %q (%v)", c.HarnessHash, h.Hash, err)
	}

	if s := mined["pr:squash"]; s.Repos[0].Base != parent(squash) || s.TestFiles != 1 || s.SourceFiles != 1 {
		t.Errorf("squash: %+v", s)
	}

	split := mined["pr:split"]
	if split.Scope != ScopeSplit || len(split.Repos) != 2 {
		t.Fatalf("split: %+v", split)
	}
	if ctxRepo := split.Repos[1]; ctxRepo.Repo != "github.com/acme/web" || ctxRepo.Role != RoleContext || ctxRepo.Base != webMerge {
		t.Errorf("split context repo: %+v", ctxRepo)
	}

	if reg := mined["fix:ok"]; reg.Kind != Regression || reg.Repos[0].Base != parent(fix) || reg.TestFiles != 1 {
		t.Errorf("regression: %+v", reg)
	}
	if st := mined["steering:ok"]; st.Repos[0].Base != sourceOnly || st.Repos[0].Merged == nil || *st.Repos[0].Merged != capability {
		t.Errorf("steering: %+v", st)
	}
}

// Multi-repository scope needs the recipe's links; the related repositories are then sealed at
// their own bases.
func TestMineMultiNeedsLinks(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	r := newRepo(t)
	r.commit("start", map[string]string{
		".casebox/casebox.yml": "version: 1\nenvironment:\n  image: golang:1.26\n  test:\n    - command: go test ./...\n  links: [go.work]\ncases:\n  test_globs: [\"checks/**\"]\n",
		"lib.go":               "package lib\n",
	})
	merge := r.pull("feature", map[string]string{"lib.go": "package lib\n\nfunc A() {}\n", "checks/a.txt": "check\n"})
	other := newRepo(t)
	other.commit("start", map[string]string{"main.go": "package main\n"})
	otherMerge := other.pull("part", map[string]string{"main.go": "package main\n\nfunc main() {}\n"})

	c := Candidate{Key: "pr:multi", Kind: Capability, Rank: 1,
		Pulls:   []Pull{{Repo: apiRepo, Number: 1, BaseRef: "main", MergeSha: merge, MergedAt: now}},
		Related: []Pull{{Repo: "github.com/acme/tool", Number: 2, BaseRef: "main", MergeSha: otherMerge, MergedAt: now}}}
	res, err := mine(ctx, opener(map[string]*gitRepo{apiRepo: r, "github.com/acme/tool": other}), MinePayload{Repo: apiRepo, Candidates: []Candidate{c}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cases) != 1 {
		t.Fatalf("mined %+v", res)
	}
	m := res.Cases[0]
	// The custom test globs make checks/a.txt the test file.
	if m.Scope != ScopeMulti || m.TestFiles != 1 || m.SourceFiles != 1 {
		t.Fatalf("multi: %+v", m)
	}
	tool := m.Repos[1]
	if tool.Role != RoleSealed || tool.Base != other.git("rev-parse", otherMerge+"^1") || *tool.Merged != otherMerge {
		t.Errorf("the related repository: %+v", tool)
	}
}

func TestSplitChange(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	// A tab in a path makes git quote it; Windows has no such file names.
	weird := "weird name\t.go"
	if runtime.GOOS == "windows" {
		weird = "weird name é.go"
	}
	base := r.commit("base", map[string]string{"store/store.go": "package store\n", "old.txt": "old\n", weird: "package x\n"})
	merged := r.commit("change", map[string]string{
		"store/store.go":      "package store\n\nfunc Get() {}\n",
		"store/store_test.go": "package store\n",
		"logo.png":            "\x89PNG\r\n\x1a\n\x00\x00binary",
		"old.txt":             "",
		weird:                 "package y\n",
		"src/Api.Tests/A.cs":  "class A {}\n",
	})
	d, err := diff(ctx, r.dir, base, merged, "")
	if err != nil {
		t.Fatal(err)
	}
	parts, err := SplitPatch(d)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	var joined []byte
	for _, p := range parts {
		paths = append(paths, p.Path)
		joined = append(joined, p.Body...)
	}
	if want := "logo.png,old.txt,src/Api.Tests/A.cs,store/store.go,store/store_test.go," + weird; strings.Join(paths, ",") != want {
		t.Fatalf("paths %q, want %q", strings.Join(paths, ","), want)
	}
	if string(joined) != string(d) {
		t.Fatal("the parts do not add up to the diff")
	}

	c, err := splitChange(d, "", repo.DefaultTestGlobs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.TestFiles, ",") != "src/Api.Tests/A.cs,store/store_test.go" {
		t.Errorf("test files %v", c.TestFiles)
	}
	if strings.Join(c.SourceFiles, ",") != "logo.png,old.txt,store/store.go,"+weird {
		t.Errorf("source files %v", c.SourceFiles)
	}
	if strings.Join(c.Dirs, ",") != ".,src/Api.Tests,store" {
		t.Errorf("dirs %v", c.Dirs)
	}
	// Each patch applies on its own to the base: the source patch, then the test patch.
	r.git("checkout", "-q", base)
	for name, patch := range map[string][]byte{"source": c.Source, "tests": c.Tests} {
		f := filepath.Join(t.TempDir(), name+".diff")
		if err := os.WriteFile(f, patch, 0o644); err != nil {
			t.Fatal(err)
		}
		r.git("apply", "--index", f)
	}
	if got := r.git("diff", "--cached", "--stat", merged); got != "" {
		t.Errorf("source and test patches do not rebuild the merged tree:\n%s", got)
	}

	// A prefixed diff (multi scope) keeps the prefix in its paths and classifies without it.
	pd, err := diff(ctx, r.dir, base, merged, "api/")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := splitChange(pd, "api/", repo.DefaultTestGlobs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pc.TestFiles, ",") != "api/src/Api.Tests/A.cs,api/store/store_test.go" || strings.Join(pc.Dirs, ",") != ".,src/Api.Tests,store" {
		t.Errorf("prefixed: %v %v", pc.TestFiles, pc.Dirs)
	}
}

func TestDefaultTestGlobs(t *testing.T) {
	for file, want := range map[string]bool{
		"store/store_test.go":                 true,
		"store/store.go":                      false,
		"src/Api/StoreTests.cs":               true,
		"src/Api/StoreTest.cs":                true,
		"src/Api.Tests/Fixtures/Data.cs":      true,
		"src/Api.IntegrationTests/Program.cs": true,
		"src/Api/Store.cs":                    false,
		"web/src/app.test.tsx":                true,
		"web/src/app.spec.ts":                 true,
		"web/src/__tests__/helpers.ts":        true,
		"web/src/app.tsx":                     false,
		"pkg/test_store.py":                   true,
		"pkg/store_test.py":                   true,
		"tests/conftest.py":                   true,
		"pkg/store.py":                        false,
		"src/test/java/com/acme/StoreIT.java": true,
		"src/main/java/com/acme/Store.java":   false,
		"latest.go":                           false,
	} {
		if got := repo.IsTestFile(repo.DefaultTestGlobs, file); got != want {
			t.Errorf("%s: test file %v, want %v", file, got, want)
		}
	}
}
