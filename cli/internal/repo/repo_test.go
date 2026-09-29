package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/alternayte/casebox/cli/internal/capture"
)

func TestRemotesNormalizeToHostOwnerName(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:Acme/Payments-API.git":             "github.com/acme/payments-api",
		"https://github.com/acme/payments-api":             "github.com/acme/payments-api",
		"https://token@github.com/acme/payments-api.git":   "github.com/acme/payments-api",
		"ssh://git@github.example.com:2222/acme/tools.git": "github.example.com/acme/tools",
		// Azure DevOps Server: the collection and project stay, /_git goes, as the server names it.
		"https://ado.example.com/tfs/DefaultCollection/Payments/_git/payments-api":     "ado.example.com/tfs/defaultcollection/payments/payments-api",
		"https://nate@ado.example.com:8443/tfs/DefaultCollection/Payments/_git/api":    "ado.example.com/tfs/defaultcollection/payments/api",
		"ssh://ado.example.com:22/tfs/DefaultCollection/Payments/_git/payments-api":    "ado.example.com/tfs/defaultcollection/payments/payments-api",
		"http://build01:8080/tfs/DefaultCollection/My%20Project/_git/My%20Project.git": "build01/tfs/defaultcollection/my%20project/my%20project",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

// The collection URL init offers must be the one the server's API lives at, or the token check fails.
func TestAzureDevOpsRemotesNameTheirCollection(t *testing.T) {
	for in, want := range map[string]string{
		"https://ado.example.com/tfs/DefaultCollection/Payments/_git/payments-api":  "https://ado.example.com/tfs/DefaultCollection",
		"https://nate@ado.example.com:8443/tfs/DefaultCollection/Payments/_git/api": "https://ado.example.com:8443/tfs/DefaultCollection",
		"http://build01:8080/tfs/DefaultCollection/_git/payments":                   "http://build01:8080/tfs/DefaultCollection",
		"https://ado.example.com/Main/Payments/_git/api":                            "https://ado.example.com/Main",
		"https://ado.example.com/Main/_git/Payments":                                "https://ado.example.com/Main",
		"ssh://ado.example.com:22/tfs/DefaultCollection/Payments/_git/payments-api": "https://ado.example.com/tfs/DefaultCollection",
	} {
		got, ok := AzureDevOpsCollection(in)
		if !ok || got != want {
			t.Errorf("AzureDevOpsCollection(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	if _, ok := AzureDevOpsCollection("git@github.com:acme/app.git"); ok {
		t.Error("a GitHub remote named a collection")
	}
}

func TestHarnessGlobsMatchWholePaths(t *testing.T) {
	for _, c := range []struct {
		pattern, file string
		want          bool
	}{
		{"AGENTS.md", "AGENTS.md", true},
		{"AGENTS.md", "docs/AGENTS.md", false},
		{"**/AGENTS.md", "docs/AGENTS.md", true},
		{"**/AGENTS.md", "AGENTS.md", true},
		{".claude/skills/**", ".claude/skills/review/SKILL.md", true},
		{".claude/skills/**", ".claude/skills", false},
		{".claude/skills/**", ".claude/settings.json", false},
		{".cursor/rules/*.mdc", ".cursor/rules/go.mdc", true},
		{".cursor/rules/*.mdc", ".cursor/rules/sub/go.mdc", false},
		{"docs/**/*.md", "docs/a/b/c.md", true},
	} {
		if got := MatchGlob(c.pattern, c.file); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
}

// The harness hash depends only on the matched files' paths and contents, so two commits with the
// same harness share a version and an unrelated change does not create a new one.
func TestTheHarnessHashChangesOnlyWithTheHarnessFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=d@example.com", "-c", "user.name=D", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", name)
	}
	run("init", "-q")
	write("AGENTS.md", "rules")
	write(".claude/skills/review/SKILL.md", "review")
	write("main.go", "package main")
	run("commit", "-q", "-m", "one")
	first := run("rev-parse", "HEAD")
	write("main.go", "package main // changed")
	run("commit", "-q", "-m", "two")
	second := run("rev-parse", "HEAD")
	write("AGENTS.md", "rules, changed")
	run("commit", "-q", "-m", "three")
	third := run("rev-parse", "HEAD")

	harness := func(commit string) *capture.Harness {
		t.Helper()
		h, err := Harness(ctx, dir, commit, DefaultHarnessGlobs)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	a, b, c := harness(first), harness(second), harness(third)
	if want := []string{".claude/skills/review/SKILL.md", "AGENTS.md"}; strings.Join(a.Files, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", a.Files, want)
	}
	blobs := run("ls-tree", "-r", first, "--", ".claude/skills/review/SKILL.md", "AGENTS.md")
	var lines []string
	for _, l := range strings.Split(blobs, "\n") {
		meta, path, _ := strings.Cut(l, "\t")
		lines = append(lines, path+" "+strings.Fields(meta)[2]+"\n")
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	if a.Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %s, want the SHA-256 of the sorted path and blob lines", a.Hash)
	}
	if a.Hash != b.Hash {
		t.Fatal("a change outside the harness changed the hash")
	}
	if b.Hash == c.Hash {
		t.Fatal("a change to AGENTS.md kept the hash")
	}
}
