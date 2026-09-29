package cases

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// instructionRun runs a case.instruction job against a fake server that serves source and the
// blobs, and a fake Anthropic model that answers reply. It returns the result and the model's
// request bodies.
func instructionRun(t *testing.T, kind, source, reply string) (InstructionResult, []string) {
	t.Helper()
	sourcePatch := patch("calc/calc.go", "~package calc", "func Add(a, b int) int { return a + b }")
	testPatch := patch("calc/calc_test.go", "func TestAdd(t *testing.T) { _ = Add(2, 3) }")
	store := blobs{}
	ctx := context.Background()
	sp, _ := store.put(ctx, "", []byte(sourcePatch))
	tp, _ := store.put(ctx, "", []byte(testPatch))
	o, _ := json.Marshal(Oracle{Kind: kind, TestFiles: []string{"calc/calc_test.go"}, SourcePatch: sp, TestPatch: tp})
	oracleHash, _ := store.put(ctx, "", o)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer worker-token" {
			t.Errorf("%s without the worker token", r.URL.Path)
		}
		switch {
		case r.URL.Path == "/worker/v1/cases/c1/source":
			_, _ = w.Write([]byte(source))
		case strings.HasPrefix(r.URL.Path, "/worker/v1/blobs/"):
			body, ok := store[strings.TrimPrefix(r.URL.Path, "/worker/v1/blobs/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
		default:
			t.Errorf("unexpected call %s", r.URL.Path)
		}
	}))
	defer server.Close()
	var requests []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, string(body))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":       "claude-test",
			"stop_reason": "tool_use",
			"content":     []map[string]any{{"type": "tool_use", "name": "instruction", "input": json.RawMessage(reply)}},
		})
	}))
	defer model.Close()

	client := analysis.New(analysis.Config{Provider: analysis.Anthropic, Model: "claude-test", BaseURL: model.URL, APIKey: "k"})
	client.Backoff = time.Millisecond
	jobs := Jobs{Client: api.New(server.URL, "worker-token"), Model: client}
	payload, _ := json.Marshal(InstructionPayload{CaseID: "c1", Kind: kind, Oracle: oracleHash})
	out, err := jobs.Instruction(ctx, worker.Job{ID: "j1", Kind: "case.instruction", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return out.(InstructionResult), requests
}

func TestInstructionFromThePullRequestTitle(t *testing.T) {
	res, requests := instructionRun(t, Capability,
		`{"kind":"capability","workItem":null,"pullTitle":"Fix Add to sum its arguments","steering":null}`,
		`{"instruction":"Adding two numbers gives the wrong result. Make addition correct."}`)
	if len(requests) != 1 {
		t.Fatalf("%d model calls", len(requests))
	}
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(requests[0]), &req); err != nil {
		t.Fatal(err)
	}
	user := req.Messages[0].Content
	// The model sees the title and the changed paths, never the diff.
	if !strings.Contains(user, "Fix Add to sum its arguments") || !strings.Contains(user, "- calc/calc.go\n") || !strings.Contains(user, "- calc/calc_test.go\n") {
		t.Errorf("the prompt lacks the title or the paths:\n%s", user)
	}
	if strings.Contains(user, "return a + b") || strings.Contains(user, "TestAdd") {
		t.Errorf("the prompt holds the diff:\n%s", user)
	}
	want := FromPullTitle + "\n\nAdding two numbers gives the wrong result. Make addition correct.\n\n## " + SignaturesHeading + "\n\n```\nfunc Add(a, b int) int\n```"
	if res.Text != want {
		t.Errorf("text:\n%s\nwant:\n%s", res.Text, want)
	}
	if strings.Join(res.Signatures, "|") != "func Add(a, b int) int" || res.Model != "claude-test" || res.Assertions != nil || res.Judge != nil {
		t.Errorf("result %+v", res)
	}
}

func TestSteeringInstructionAssertions(t *testing.T) {
	res, requests := instructionRun(t, Steering,
		`{"kind":"steering","workItem":null,"pullTitle":null,"steering":{"prompts":["Make addition correct in the calculator."],
		  "window":{"interventionId":"e:1","signal":"follow_up","phase":"in_session","human":"you did not run the tests","files":["calc/calc.go"]}}}`,
		`{"instruction":"Make addition correct in the calculator.",
		  "assertions":[{"kind":"command_before_done","pattern":"go test"},{"kind":"forbidden_file"},{"kind":"diff_must_not_match","pattern":"("}],
		  "judge":[{"question":"Did the agent run the tests before it said it was done?"},{"question":"Second?"}]}`)
	if !strings.Contains(requests[0], "you did not run the tests") || !strings.Contains(requests[0], `"assertions"`) {
		t.Errorf("the steering request lacks the correction or the assertion schema:\n%s", requests[0])
	}
	if strings.HasPrefix(res.Text, FromPullTitle) {
		t.Errorf("a steering instruction says it came from a title:\n%s", res.Text)
	}
	if len(res.Assertions) != 1 || res.Assertions[0] != (Assertion{Kind: "command_before_done", Pattern: "go test"}) {
		t.Errorf("assertions %+v", res.Assertions)
	}
	if len(res.Judge) != 1 || res.Judge[0].Question != "Did the agent run the tests before it said it was done?" {
		t.Errorf("judge %+v", res.Judge)
	}
}
