package spool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
)

func session(id string) capture.Session {
	return capture.Session{ID: "claude-code:" + id, Agent: "claude-code", Source: "import", Person: "⟦cbx:email:dev@example.com⟧", StartedAt: time.Unix(1_700_000_000, 0).UTC()}
}

func events(from, n int, text string) []capture.Event {
	out := make([]capture.Event, n)
	for i := range out {
		out[i] = capture.Event{Seq: int64(from + i), At: time.Unix(1_700_000_000, 0).UTC(), Kind: capture.KindPrompt, Text: text}
	}
	return out
}

func TestAddingTheSameHistoryTwiceAddsNothing(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "spool.db"), DefaultCap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 2 {
		if err := s.Add(ctx, session("a"), events(0, 10, "x")); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 10 {
		t.Fatalf("pending = %d, want 10", st.Pending)
	}
}

func TestTheCapDropsOnlyAcknowledgedEventsOldestFirst(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "spool.db"), 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	big := strings.Repeat("y", 2000)

	if err := s.Add(ctx, session("old"), events(0, 100, big)); err != nil {
		t.Fatal(err)
	}
	seqs := make([]int64, 100)
	for i := range seqs {
		seqs[i] = int64(i)
	}
	if err := s.Ack(ctx, "claude-code:old", seqs); err != nil {
		t.Fatal(err)
	}
	// Twice the cap of unacknowledged data: all of it must stay.
	if err := s.Add(ctx, session("new"), events(0, 250, big)); err != nil {
		t.Fatal(err)
	}

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 250 {
		t.Fatalf("pending = %d, want all 250 unacknowledged events kept", st.Pending)
	}
	if st.Acked != 0 {
		t.Fatalf("acked = %d, want the acknowledged events dropped to make room", st.Acked)
	}
}

func TestPendingBatchesStayWithinTheirLimits(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "spool.db"), DefaultCap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Add(ctx, session("a"), events(0, 30, strings.Repeat("z", 100))); err != nil {
		t.Fatal(err)
	}
	batches, err := s.Pending(ctx, 10, 20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || len(batches[0].Events) != 20 || batches[0].Events[0].Seq != 0 {
		t.Fatalf("got %d batches; want one batch of the first 20 events in order", len(batches))
	}
}

// The helper process writes until it is killed; the spool must reopen intact.
func TestTheSpoolSurvivesAKilledProcess(t *testing.T) {
	if path := os.Getenv("CASEBOX_SPOOL_WRITER"); path != "" {
		s, err := Open(path, DefaultCap)
		if err != nil {
			panic(err)
		}
		for i := 0; ; i++ {
			if err := s.Add(context.Background(), session(fmt.Sprint(i)), events(0, 50, strings.Repeat("w", 500))); err != nil {
				panic(err)
			}
		}
	}

	path := filepath.Join(t.TempDir(), "spool.db")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestTheSpoolSurvivesAKilledProcess$")
	cmd.Env = append(os.Environ(), "CASEBOX_SPOOL_WRITER="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	s, err := Open(path, DefaultCap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var check string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
	var partial int
	if err := s.db.QueryRow(`SELECT count(*) FROM (SELECT session_id FROM events GROUP BY session_id HAVING count(*) <> 50)`).Scan(&partial); err != nil {
		t.Fatal(err)
	}
	if partial != 0 {
		t.Fatalf("%d sessions hold part of a write; each write must commit whole or not at all", partial)
	}
	st, err := s.Stats(context.Background())
	if err != nil || st.Pending == 0 {
		t.Fatalf("stats = %+v, %v; want the committed writes kept", st, err)
	}
	if err := s.Add(context.Background(), session("after"), events(0, 1, "x")); err != nil {
		t.Fatalf("the spool does not accept writes after the kill: %v", err)
	}
}
