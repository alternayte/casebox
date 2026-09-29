package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alternayte/casebox/cli/internal/api"
)

// fakeCases serves the review routes and records what the CLI changed.
type fakeCases struct {
	mu    sync.Mutex
	queue []caseView
	calls []string
}

func (f *fakeCases) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodGet && path == "/api/v1/cases/queue":
		reply(f.queue)
		return
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/oracle"):
		reply(map[string]any{"tests": map[string][]string{"failToPass": {"store::TestPut"}, "passToPass": {"store::TestGet"}}})
		return
	case r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "/api/v1/cases/")
		for _, c := range f.queue {
			if c.Summary.ID == id {
				reply(c)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	f.calls = append(f.calls, strings.TrimSpace(r.Method+" "+path+" "+strings.TrimSpace(string(body))))
	switch {
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/instruction"):
		var b struct{ Text string }
		_ = json.Unmarshal(body, &b)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/cases/"), "/instruction")
		for i := range f.queue {
			if f.queue[i].Summary.ID == id {
				f.queue[i].Instruction = &b.Text
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "/api/v1/cases/approvals":
		reply(map[string]any{"approved": []string{"c1"}, "refused": []map[string]string{{"id": "c2", "reason": "A steering case needs approved assertions first."}}})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func queued(id, kind, instruction string, blocker string) caseView {
	two, forty := 2, 40
	c := caseView{Summary: caseSummary{ID: id, Kind: kind, Workspace: "shop", Status: "validated", Scope: "single", Source: "github.com/acme/shop#7", FailToPass: &two, PassToPass: &forty, Weight: 1}}
	c.Instruction = &instruction
	c.Signatures = []string{"func Put(key, value string)"}
	if blocker != "" {
		c.ApprovalBlocker = &blocker
	}
	return c
}

func TestReviewWalksTheQueueWithTheAnswersFromStdin(t *testing.T) {
	fake := &fakeCases{queue: []caseView{
		queued("c1", "capability", "Add a put.", ""),
		queued("c2", "steering", "Run the tests before done.", "A steering case needs approved assertions first."),
		queued("c3", "regression", "Fix the crash.", ""),
		queued("c4", "capability", "Left for later.", ""),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	var out strings.Builder
	edited := ""
	r := reviewer{
		client: api.New(srv.URL, "cli"),
		// c1: edit, then approve. c2: approve is refused locally, then reject with a reason.
		// c3: skip. c4: quit.
		in:  bufio.NewReader(strings.NewReader("e\na\na\nr\n\nr\nwrong area\ns\nq\n")),
		out: &out,
		edit: func(text string) (string, error) {
			edited = text
			return "Add a put for a key and a value.\n", nil
		},
	}
	if err := r.walk(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if edited != "Add a put." {
		t.Errorf("the editor got %q, want the current instruction", edited)
	}
	want := []string{
		`PUT /api/v1/cases/c1/instruction {"text":"Add a put for a key and a value."}`,
		`POST /api/v1/cases/c1/approval`,
		`POST /api/v1/cases/c2/rejection {"reason":"wrong area"}`,
	}
	if strings.Join(fake.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls\n got %q\nwant %q", fake.calls, want)
	}
	text := out.String()
	for _, s := range []string{
		"Source: github.com/acme/shop#7",
		"Tests: 2 fail-to-pass, 40 pass-to-pass",
		"store::TestPut",
		"func Put(key, value string)",
		"The instruction now reads:\n    Add a put for a key and a value.",
		"Approval is not possible: A steering case needs approved assertions first.\n[e]dit, [r]eject, [s]kip, [q]uit? ",
		"Approved 1, rejected 1, skipped 1.",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("the output lacks %q:\n%s", s, text)
		}
	}
}

func TestApproveAllSendsTheQueueAndListsWhatTheServerRefused(t *testing.T) {
	fake := &fakeCases{queue: []caseView{queued("c1", "capability", "Add a put.", ""), queued("c2", "steering", "Run the tests.", "A steering case needs approved assertions first.")}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	var out strings.Builder
	r := reviewer{client: api.New(srv.URL, "cli"), in: bufio.NewReader(strings.NewReader("")), out: &out}
	if err := r.approveAll(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != `POST /api/v1/cases/approvals {"ids":["c1","c2"]}` {
		t.Errorf("calls = %q", fake.calls)
	}
	want := "Approved 1 of 2 cases.\nRefused 1:\n  c2: A steering case needs approved assertions first.\n"
	if out.String() != want {
		t.Errorf("output\n got %q\nwant %q", out.String(), want)
	}
}
