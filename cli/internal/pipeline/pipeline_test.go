package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
)

func enrolledRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("CASEBOX_HOME", t.TempDir())
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "dev@example.com"}, {"config", "user.name", "Ada Lovelace"},
		{"remote", "add", "origin", "git@github.com:acme/app.git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".casebox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".casebox", "casebox.yml"), []byte("version: 1\ncapture:\n  redact: ['ACME-[0-9]{4}']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
	return dir
}

// What leaves the machine depends on the prompt mode; identities and secrets never leave raw.
func TestThePromptModeDecidesWhatTextLeavesTheMachine(t *testing.T) {
	dir := enrolledRepo(t)
	r, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	session := r.Session(context.Background(), capture.Session{ID: "codex:1", Agent: "codex", Source: "import", StartedAt: time.Now()})
	if session.Repo != "github.com/acme/app" || session.Person != "⟦cbx:email:dev@example.com⟧" {
		t.Fatalf("session = %+v", session)
	}
	events := []capture.Event{
		{Kind: capture.KindPrompt, Text: "Ada Lovelace says use ACME-1234 and token=abcdef123"},
		{Kind: capture.KindToolResult, Text: "file body", Tool: &capture.Tool{Name: "Read", Files: []string{filepath.Join(dir, "src", "main.go")}}},
	}

	off := r.Events(events, ModeOff)
	if off[0].Text != "" || off[1].Text != "" {
		t.Fatalf("mode off kept text: %+v", off)
	}
	redacted := r.Events(events, ModeRedacted)
	want := "⟦cbx:name:Ada Lovelace⟧ says use [redacted:team] and token=[redacted:assignment]"
	if redacted[0].Text != want {
		t.Fatalf("redacted prompt\n got %q\nwant %q", redacted[0].Text, want)
	}
	if redacted[1].Text != "" {
		t.Fatalf("mode redacted kept tool output: %q", redacted[1].Text)
	}
	if got := redacted[1].Tool.Files[0]; got != "src/main.go" {
		t.Fatalf("file path = %q, want it relative to the repository", got)
	}
	if full := r.Events(events, ModeFull); !strings.Contains(full[1].Text, "file body") {
		t.Fatalf("mode full dropped tool output")
	}
}

// A session's commits are the developer's own, from its start to 30 minutes after its end; the
// abandonment rate depends on this count.
func TestASessionCountsTheDevelopersCommitsUntilHalfAnHourAfterItsEnd(t *testing.T) {
	dir := enrolledRepo(t)
	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	commit := func(email string, at time.Time) {
		t.Helper()
		cmd := exec.Command("git", "-C", dir, "-c", "user.email="+email, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "work")
		date := at.Format(time.RFC3339)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v %s", err, out)
		}
	}
	commit("dev@example.com", start.Add(-10*time.Minute))
	commit("dev@example.com", start.Add(20*time.Minute))
	commit("other@example.com", start.Add(30*time.Minute))
	commit("dev@example.com", start.Add(80*time.Minute))
	commit("dev@example.com", start.Add(2*time.Hour))

	r, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	open := r.Session(context.Background(), capture.Session{ID: "codex:2", Agent: "codex", StartedAt: start})
	if open.Commits != nil {
		t.Fatalf("a session without an end has commits = %d, want unknown", *open.Commits)
	}
	if open.Harness == nil || len(open.Harness.Files) != 0 {
		t.Fatalf("harness = %+v, want an empty harness read from the commit before the start", open.Harness)
	}
	end := start.Add(time.Hour)
	ended := r.Session(context.Background(), capture.Session{ID: "codex:2", Agent: "codex", StartedAt: start, EndedAt: &end})
	if ended.Commits == nil || *ended.Commits != 2 {
		t.Fatalf("commits = %v, want 2", ended.Commits)
	}
}
