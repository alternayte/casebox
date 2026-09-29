package entire

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/gitmirror"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=dev@example.com", "-c", "user.name=Dev", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A checkpoint ref (Entire 0.10+) with one Codex session, and a code commit whose trailer names it.
func TestACheckpointBecomesTheSessionOfTheCommitThatNamesIt(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")

	const id = "01M228EVE8JJQ9J34JESFQX7BH"
	tree := t.TempDir()
	files := map[string]string{
		"metadata.json":      `{"checkpoint_id":"` + id + `"}`,
		"0/metadata.json":    `{"session_id":"sess-1","agent":"Codex","model":"gpt-6","branch":"fix/login","created_at":"2026-09-20T10:00:00Z"}`,
		"0/transcript.jsonl": `{"v":1,"agent":"codex","type":"user","ts":"2026-09-20T10:00:01Z","content":[{"id":"u1","text":"fix the login"}]}` + "\n" + `{"v":1,"agent":"codex","type":"assistant","ts":"2026-09-20T10:00:05Z","id":"m1","input_tokens":30,"output_tokens":7,"content":[{"type":"text","text":"Done."},{"type":"tool_use","id":"t1","name":"apply_patch","input":{"file_path":"src/login.go"},"result":{"output":"ok","status":"success"}}]}` + "\n",
	}
	for name, content := range files {
		path := filepath.Join(tree, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Build the checkpoint commit from the files, outside the code history.
	index := filepath.Join(t.TempDir(), "index")
	add := exec.Command("git", "-C", dir, "--work-tree", tree, "add", "-A", ".")
	add.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	write := exec.Command("git", "-C", dir, "write-tree")
	write.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	treeSHA, err := write.Output()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := git(t, dir, "commit-tree", strings.TrimSpace(string(treeSHA)), "-m", "Checkpoint: "+id)
	git(t, dir, "update-ref", "refs/entire/checkpoints/"+id[len(id)-2:]+"/"+id, checkpoint)
	// A leftover of Entire's abandoned v2 experiment must be ignored.
	git(t, dir, "update-ref", "refs/entire/checkpoints/v2/main", checkpoint)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "Fix login\n\nEntire-Checkpoint: "+id)

	sessions, err := Read(context.Background(), &gitmirror.Mirror{Dir: dir}, time.Now().AddDate(0, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.Session.ID != "codex:sess-1" || s.Session.Model != "gpt-6" || s.Session.Branch != "fix/login" || s.AuthorEmail != "dev@example.com" {
		t.Fatalf("session = %+v, author %q", s.Session, s.AuthorEmail)
	}
	var kinds []string
	for _, e := range s.Events {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "prompt,response,tool_call,tool_result" {
		t.Fatalf("kinds = %s", got)
	}
	if s.Events[0].Seq != 3_000_000_000 || s.Events[1].Usage.InputTokens != 30 || s.Events[2].Tool.Files[0] != "src/login.go" {
		t.Fatalf("events = %+v", s.Events)
	}
}
