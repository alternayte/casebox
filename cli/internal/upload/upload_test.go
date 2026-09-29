package upload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/spool"
)

// An interrupted upload resumes, and nothing is acknowledged that the server did not store.
func TestAnInterruptedUploadResumesWithoutLosingOrRepeatingEvents(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	stored := map[int64]int{}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var b spool.Batch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		for _, e := range b.Events {
			stored[e.Seq]++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := spool.Open(filepath.Join(t.TempDir(), "spool.db"), spool.DefaultCap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	session := capture.Session{ID: "codex:1", Agent: "codex", Source: "import", Person: "⟦cbx:email:d@example.com⟧", StartedAt: time.Now().UTC()}
	var events []capture.Event
	for i := range 1200 {
		events = append(events, capture.Event{Seq: int64(i), At: time.Now().UTC(), Kind: capture.KindPrompt})
	}
	if err := s.Add(ctx, session, events); err != nil {
		t.Fatal(err)
	}

	client := api.New(srv.URL, "cbx_ingest_x")
	if _, err := Run(ctx, client, s); err == nil {
		t.Fatal("the first run should stop at the failed request")
	}
	if _, err := Run(ctx, client, s); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, client, s); err != nil {
		t.Fatal(err)
	}

	if len(stored) != 1200 {
		t.Fatalf("the server stored %d distinct events, want 1200", len(stored))
	}
	for seq, n := range stored {
		if n != 1 {
			t.Fatalf("event %d was sent %d times after it was acknowledged", seq, n)
		}
	}
	st, _ := s.Stats(ctx)
	if st.Pending != 0 {
		t.Fatalf("%d events are still pending", st.Pending)
	}
}
