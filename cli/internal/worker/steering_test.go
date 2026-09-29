package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/gitmirror"
)

type prRepo struct {
	t   *testing.T
	dir string
	at  time.Time
}

func (r *prRepo) commit(file string, lines []string, msg string) (string, time.Time) {
	r.t.Helper()
	r.at = r.at.Add(time.Hour)
	if err := os.WriteFile(filepath.Join(r.dir, file), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	for _, args := range [][]string{{"add", file}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.email=d@example.com", "-c", "user.name=D", "-c", "commit.gpgsign=false"}, args...)...)
		date := r.at.Format(time.RFC3339)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			r.t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", r.dir, "rev-parse", "HEAD").Output()
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), r.at
}

func numbered(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "line " + string(rune('a'+i))
	}
	return lines
}

// A human commit counts as a rewrite only for the lines it changes that an agent commit of the
// pull request wrote; a review comment is answered by the first later commit near its line.
func TestPullRequestRewritesAndReviewChanges(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	r := &prRepo{t: t, dir: dir, at: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)}

	lines := numbered(12)
	r.commit("a.go", lines, "base")
	lines[2], lines[3], lines[4], lines[5] = "agent 3", "agent 4", "agent 5", "agent 6"
	agentSHA, agentAt := r.commit("a.go", lines, "agent: rewrite the middle")
	commentAt := agentAt.Add(10 * time.Minute)
	lines[10] = "human 11" // far from the comment, and a base line: no rewrite
	farSHA, farAt := r.commit("a.go", lines, "human: unrelated change")
	lines[5], lines[8] = "human 6", "human 9" // one agent line, one base line
	humanSHA, humanAt := r.commit("a.go", lines, "human: fix the agent's line")

	commits := []prCommit{
		{SHA: humanSHA, At: humanAt},
		{SHA: agentSHA, Agent: true, At: agentAt},
		{SHA: farSHA, At: farAt},
	}
	m := &gitmirror.Mirror{Dir: dir}
	rw, err := rewrites(ctx, m, commits)
	if err != nil {
		t.Fatal(err)
	}
	if len(rw) != 1 || rw[0].SHA != humanSHA || rw[0].Lines != 1 || strings.Join(rw[0].Files, ",") != "a.go" {
		t.Fatalf("rewrites = %+v, want one line of a.go in %s", rw, humanSHA)
	}

	comments := []prComment{
		{ID: json.RawMessage(`101`), Path: "a.go", Line: 6, CommitSHA: agentSHA, At: commentAt},
		{ID: json.RawMessage(`102`), Path: "a.go", Line: 1, CommitSHA: agentSHA, At: commentAt},
		{ID: json.RawMessage(`103`), Path: "b.go", Line: 4, CommitSHA: agentSHA, At: commentAt},
	}
	rc := reviewChanges(ctx, m, commits, comments)
	if len(rc) != 1 || string(rc[0].CommentID) != "101" || rc[0].SHA != humanSHA {
		t.Fatalf("review changes = %+v, want comment 101 answered by %s", rc, humanSHA)
	}
}

// The classify job returns one result per window the model may see, with the labels as the model
// gave them, even invalid ones, which the server validates. A window with nothing to classify gets
// no result, and the task type comes once.
func TestClassifyReturnsTheSpecResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker/v1/steering/windows":
			_, _ = w.Write([]byte(`{"windows":[
				{"interventionId":"e:1","signal":"follow_up","phase":"in_session","ruleIntent":null,"human":"you did not run the tests","before":{"text":"Done.","tools":[]},"after":null,"files":[],"repo":"github.com/acme/app","commits":[]},
				{"interventionId":"e:2","signal":"interruption","phase":"in_session","ruleIntent":null,"human":null,"before":{"text":"","tools":[]},"after":null,"files":[],"repo":"github.com/acme/app","commits":[]},
				{"interventionId":"e:3","signal":"follow_up","phase":"in_session","ruleIntent":null,"human":"INVALID please","before":null,"after":null,"files":[],"repo":"github.com/acme/app","commits":[]}],
				"task":{"sessionId":"claude-code:1","text":"Fix the rounding bug in invoices"}}`))
		case "/worker/v1/steering/examples":
			_, _ = w.Write([]byte(`{"examples":[]}`))
		default:
			t.Errorf("unexpected call %s", r.URL.Path)
		}
	}))
	defer server.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		input := `{"reason":"r","intent":"correction","wentWrong":"unverified_done","wentWrongLabel":null,"prevention":"verification","confidence":0.9}`
		name := "label"
		switch {
		case strings.Contains(string(body), `"task_type"`):
			name, input = "task_type", `{"taskType":"bug","confidence":0.95}`
		case strings.Contains(string(body), "INVALID"):
			input = `{"reason":"r","intent":"complaint","confidence":0.9}`
		}
		_, _ = w.Write([]byte(`{"model":"claude-test-20260901","content":[{"type":"tool_use","name":"` + name + `","input":` + input + `}]}`))
	}))
	defer model.Close()

	s := Steering{
		Client:      api.New(server.URL, "w"),
		Model:       analysis.New(analysis.Config{Provider: analysis.Anthropic, Model: "claude-test", BaseURL: model.URL, APIKey: "k"}),
		Concurrency: 2,
	}
	out, err := s.Classify(context.Background(), Job{Payload: json.RawMessage(`{"stream":"steering:session:claude-code:1","interventionIds":["e:1","e:2","e:3"],"taskFor":"claude-code:1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out)
	want := `{"model":"claude-test-20260901","promptVersion":"steering-v1","results":[` +
		`{"interventionId":"e:1","intent":"correction","wentWrong":"unverified_done","prevention":"verification","confidence":0.9},` +
		`{"interventionId":"e:3","intent":"complaint","confidence":0.9}],` +
		`"taskType":"bug"}`
	if string(got) != want {
		t.Fatalf("result\n got %s\nwant %s", got, want)
	}
}
