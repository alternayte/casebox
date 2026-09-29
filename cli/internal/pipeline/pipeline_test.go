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
	session := r.Session(capture.Session{ID: "codex:1", Agent: "codex", Source: "import", StartedAt: time.Now()})
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
