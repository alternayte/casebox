package runner_test

import (
	"archive/tar"
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/runner"
	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/docker"
)

// spec has git, and an install step that leaves a dependency folder in the working directory, as
// npm ci or dotnet restore do.
var spec = sandbox.EnvSpec{
	Image:   "alpine:3.20",
	Install: []string{"apk add --no-cache git", "mkdir -p node_modules/dep web/node_modules/x && echo dep > node_modules/dep/index.js && echo x > web/node_modules/x/a.js"},
	Context: map[string][]byte{"deps.lock": []byte("lock\n")},
}

var baseFiles = map[string]string{
	"deps.lock":                  "lock\n",
	".gitignore":                 "build/\n",
	"README.md":                  "hello\n",
	"AGENTS.md":                  "base harness\n",
	".claude/skills/a/SKILL.md":  "base skill\n",
	"store/store.go":             "package store\n\nfunc Get() int { return 1 }\n",
	"store/store_test.go":        "package store\n// held out\n",
	"web/package.json":           "{}\n",
	".casebox/casebox.yml":       "version: 1\n",
	"sub/.git/config":            "[core]\n",
	"store/testdata/fixture.txt": "fixture\n",
}

var heldOut = []string{"store/store_test.go"}

func provider(t *testing.T) *docker.Provider {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := docker.New()
	if err := p.Check(ctx); err != nil {
		t.Skipf("the sealing tests need Docker, which is not usable here: %v", err)
	}
	return p
}

func TestSealing(t *testing.T) {
	p := provider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	sb, err := runner.Seal(ctx, p, spec, bytes.NewReader(tree(t, baseFiles)), heldOut, []string{"proxy.golang.org"}, nil, map[string]string{"MODEL_API_KEY": "sk-secret"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	t.Cleanup(func() { _ = p.Destroy(context.Background(), sb) })

	sh := func(script string) string {
		t.Helper()
		res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", script}, Timeout: time.Minute})
		if err != nil {
			t.Fatalf("%s: %v", script, err)
		}
		return strings.TrimSpace(string(res.Stdout))
	}

	t.Run("one commit and no history", func(t *testing.T) {
		if got := sh("git rev-list --all --count"); got != "1" {
			t.Fatalf("the repository has %s commits", got)
		}
		if got := sh("git remote"); got != "" {
			t.Fatalf("the repository has remotes %q", got)
		}
		if got := sh("git reflog; ls .git/logs 2>/dev/null"); got != "" {
			t.Fatalf("the reflog is not empty: %q", got)
		}
		if got := sh("git tag -l"); got != "" {
			t.Fatalf("the repository has tags %q", got)
		}
		if got := sh("git for-each-ref --format='%(refname)'"); got != "refs/heads/main" {
			t.Fatalf("the repository has refs %q, want only refs/heads/main (no refs/entire, no refs/notes)", got)
		}
		if got := sh("git log -1 --format='%an <%ae> %cn <%ce> %at %s'"); got != "casebox <casebox@localhost> casebox <casebox@localhost> 946684800 base" {
			t.Fatalf("the base commit is %q", got)
		}
		if got := sh("git status --porcelain --untracked-files=all"); got != "" {
			t.Fatalf("the base repository is not clean (the environment's dependency folders must be ignored): %q", got)
		}
	})

	t.Run("no held-out tests and no casebox files", func(t *testing.T) {
		for _, f := range []string{".casebox", "store/store_test.go", "sub/.git"} {
			if got := sh("test -e " + f + " && echo present"); got != "" {
				t.Fatalf("%s is in the agent's sandbox", f)
			}
		}
		if got := sh("cat store/testdata/fixture.txt"); got != "fixture" {
			t.Fatalf("a base file is missing: %q", got)
		}
	})

	t.Run("egress", func(t *testing.T) {
		for _, url := range []string{"https://github.com/", "https://example.com/"} {
			res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"wget", "-q", "-O", "/dev/null", "-T", "20", url}, Timeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode == 0 || !strings.Contains(string(res.Stderr), "403") {
				t.Fatalf("%s: exit %d, %q; want the proxy's 403", url, res.ExitCode, res.Stderr)
			}
		}
		if got := sh("nc -w 3 140.82.112.3 443 </dev/null && echo reached"); got != "" {
			t.Fatal("a raw connection to a GitHub address went around the proxy")
		}
		if _, err := runner.Seal(ctx, p, spec, bytes.NewReader(tree(t, baseFiles)), nil, []string{"*.github.com"}, nil, nil); err == nil {
			t.Fatal("Seal accepted an allow-list that reaches the git host")
		}
	})

	// The agent changes a file, adds one, writes into a dependency folder and into an ignored
	// folder, and writes its own version of the held-out test.
	sh(`set -e
printf 'package store\n\nfunc Get() int { return 2 }\n' > store/store.go
mkdir -p pkg build && printf 'package pkg\n' > pkg/new.go && printf 'bin' > build/out
printf '\000\001binary' > pkg/blob.bin
echo changed > node_modules/dep/index.js
printf 'package store\n// the agent wrote this\n' > store/store_test.go
git add pkg/new.go && git commit -q -m "agent commit"`)

	diff, err := runner.AgentDiff(ctx, p, sb)
	if err != nil {
		t.Fatalf("AgentDiff: %v", err)
	}
	for _, want := range []string{"b/store/store.go", "b/pkg/new.go", "b/pkg/blob.bin", "GIT binary patch", "b/store/store_test.go"} {
		if !bytes.Contains(diff, []byte(want)) {
			t.Fatalf("the agent's diff lacks %s:\n%s", want, diff)
		}
	}
	for _, unwanted := range []string{"node_modules", "build/out"} {
		if bytes.Contains(diff, []byte(unwanted)) {
			t.Fatalf("the agent's diff carries %s:\n%s", unwanted, diff)
		}
	}

	testPatch := []byte("diff --git a/store/store_test.go b/store/store_test.go\n" +
		"--- a/store/store_test.go\n+++ b/store/store_test.go\n@@ -1,2 +1,3 @@\n package store\n // held out\n+// merged test\n")
	commands := []repo.TestCommand{{
		Command: `set -e; cat store/store.go > /results/store.go; cat store/store_test.go > /results/test.go; test -f pkg/new.go; env > /results/env.txt`,
		Results: "junit",
	}}

	t.Run("verifier", func(t *testing.T) {
		res, err := runner.Verify(ctx, p, spec, bytes.NewReader(tree(t, baseFiles)), diff, testPatch, heldOut, commands, nil)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !res.Applied || len(res.Commands) != 1 {
			t.Fatalf("the verifier did not run: %+v", res)
		}
		c := res.Commands[0]
		if c.ExitCode != 0 {
			t.Fatalf("the test command failed: %s", c.Output)
		}
		if !strings.Contains(string(c.Files["store.go"]), "return 2") {
			t.Fatalf("the agent's change is not in the verifier: %q", c.Files["store.go"])
		}
		if got := string(c.Files["test.go"]); got != "package store\n// held out\n// merged test\n" {
			t.Fatalf("the held-out test in the verifier is %q, want the base plus the test patch", got)
		}
		if env := string(c.Files["env.txt"]); strings.Contains(env, "MODEL_API_KEY") || strings.Contains(env, "sk-secret") {
			t.Fatal("the verifier has the agent's model key")
		}
	})

	t.Run("a diff that does not apply", func(t *testing.T) {
		bad := runner.Patch{Name: "stale", Diff: []byte("diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-something else\n+new\n")}
		res, err := runner.ApplyAndRun(ctx, p, spec, bytes.NewReader(tree(t, baseFiles)), []runner.Patch{bad}, commands, nil)
		if err != nil {
			t.Fatalf("ApplyAndRun: %v", err)
		}
		if res.Applied || res.FailedPatch != "stale" || res.ApplyOutput == "" || len(res.Commands) != 0 {
			t.Fatalf("a patch that does not apply gave %+v", res)
		}
	})
}

func TestOverlay(t *testing.T) {
	p := provider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sb, err := runner.Seal(ctx, p, spec, bytes.NewReader(tree(t, baseFiles)), heldOut, nil, nil, nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	t.Cleanup(func() { _ = p.Destroy(context.Background(), sb) })
	old := sb.Meta[runner.MetaBase]
	globs := []string{"AGENTS.md", ".claude/skills/**"}

	sb, err = runner.Overlay(ctx, p, sb, map[string][]byte{"AGENTS.md": []byte("candidate harness\n")}, globs, false)
	if err != nil {
		t.Fatalf("Overlay: %v", err)
	}
	sh := func(script string) string {
		t.Helper()
		res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", script}})
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(res.Stdout))
	}
	if got := sh("cat AGENTS.md"); got != "candidate harness" {
		t.Fatalf("AGENTS.md is %q", got)
	}
	if got := sh("test -e .claude && echo present"); got != "" {
		t.Fatal("the base skill and its folders are still there")
	}
	if got := sh("git rev-list --all --count; git status --porcelain; git cat-file -t " + old + " 2>/dev/null; git reflog"); got != "1" {
		t.Fatalf("after the overlay the repository shows %q, want one commit, clean, and no trace of the old base", got)
	}
	if _, err := runner.Overlay(ctx, p, sb, map[string][]byte{"src/main.go": nil}, globs, false); err == nil {
		t.Fatal("Overlay wrote a file the harness globs do not match")
	}
	// A shared harness goes into the sandbox user's home, outside the repository and its diff.
	home := sh(`printf %s "$HOME"`)
	if err := runner.UserFiles(ctx, p, sb, home, map[string][]byte{".claude/CLAUDE.md": []byte("team rules\n"), ".claude/skills/review/SKILL.md": []byte("review\n")}); err != nil {
		t.Fatalf("UserFiles: %v", err)
	}
	if got := sh("cat ~/.claude/CLAUDE.md ~/.claude/skills/review/SKILL.md; id -u; stat -c %u ~/.claude/CLAUDE.md"); got != "team rules\nreview\n10001\n10001" {
		t.Fatalf("the shared harness in the home reads %q", got)
	}
	diff, err := runner.AgentDiff(ctx, p, sb)
	if err != nil || len(diff) != 0 {
		t.Fatalf("the overlay shows in the agent's diff: %q, %v", diff, err)
	}
}

func tree(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "0123456789abcdef0123456789abcdef01234567"}}); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
