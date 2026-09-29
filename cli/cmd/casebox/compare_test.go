package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/repo"
)

// fakeEvaluations serves the evaluation routes: each GET of the evaluation answers the next
// detail in turn, and the last one from then on.
type fakeEvaluations struct {
	mu       sync.Mutex
	estimate estimateView
	details  []evaluationDetail
	calls    []string
	bodies   map[string]evaluationRequest
}

func (f *fakeEvaluations) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/api/v1/evaluations/estimate" || r.URL.Path == "/api/v1/evaluations"):
		var req evaluationRequest
		_ = json.Unmarshal(body, &req)
		f.bodies[r.URL.Path] = req
		if r.URL.Path == "/api/v1/evaluations" {
			w.WriteHeader(http.StatusCreated)
			reply(evaluationCreated{ID: "01EVAL", Estimate: f.estimate})
			return
		}
		reply(f.estimate)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/evaluations/01EVAL":
		d := f.details[0]
		if len(f.details) > 1 {
			f.details = f.details[1:]
		}
		reply(d)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func newFakeEvaluations(needsConfirmation bool, details ...evaluationDetail) *fakeEvaluations {
	return &fakeEvaluations{
		bodies: map[string]evaluationRequest{},
		estimate: estimateView{
			Estimate: evaluationEstimate{Cases: 30, Runs: 180, BaselineTokens: 36_000_000, CandidateTokens: 36_000_000, BaselineUsd: 40.5, CandidateUsd: 67.5,
				TotalUsd: 108, SandboxMinutes: 2520, MinMinutes: 42, MaxMinutes: 2520, PerRoundUsd: 36},
			CapUsd: 150, NeedsConfirmation: needsConfirmation, MonthSpentUsd: 12.3, MonthlyUsd: 500, ConfirmAboveUsd: 50, Change: "model", Cases: 30,
		},
		details: details,
	}
}

func testRequest(t *testing.T) evaluationRequest {
	t.Helper()
	three, fifteen, five, twentyFive := 3.0, 15.0, 5.0, 25.0
	prices := map[string]repo.Price{"claude-sonnet-5": {Input: &three, Output: &fifteen}, "claude-opus-5": {Input: &five, Output: &twentyFive}}
	base := harnessSpec{Agent: "claude-code", AgentVersion: "2.4.1", Model: "claude-sonnet-5", Harness: "main"}
	cand, purpose, err := applyChange(base, "model=claude-opus-5")
	if err != nil {
		t.Fatal(err)
	}
	req, err := compareRequest("shop", base, cand, prices, purpose, 3, 0, 0.05, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func detail(status string, checkpoints []evaluationCheckpoint, verdict *verdictView, completed int, spent float64) evaluationDetail {
	return evaluationDetail{
		Evaluation: evaluationRow{ID: "01EVAL", Workspace: "shop", Split: "dev", Purpose: "compare", Status: status, Change: "model",
			Baseline:  harnessSpec{Agent: "claude-code", AgentVersion: "2.4.1", Model: "claude-sonnet-5", Harness: "main"},
			Candidate: harnessSpec{Agent: "claude-code", AgentVersion: "2.4.1", Model: "claude-opus-5", Harness: "main"},
			Repeats:   3, Delta: 0.05, CapUsd: 150, Estimate: evaluationEstimate{Runs: 180, TotalUsd: 108}, Cases: 30,
			SpentUsd: spent, RunsCompleted: completed, Verdict: verdict},
		Checkpoints: checkpoints,
	}
}

func TestCompareEstimatesConfirmsFollowsAndPrintsTheVerdict(t *testing.T) {
	round1 := evaluationCheckpoint{Round: 1, Level: 0.99, Cases: 30, Delta: 0.08, Lower: -0.02, Upper: 0.18, Verdict: "inconclusive"}
	round2 := evaluationCheckpoint{Round: 2, Level: 0.99, Cases: 30, Delta: 0.12, Lower: 0.04, Upper: 0.20, Verdict: "better"}
	costRatio, costLo, costHi := 1.32, 1.18, 1.47
	verdict := &verdictView{Verdict: "better", Delta: 0.12, Lower: 0.04, Upper: 0.20, Level: 0.99, Cases: 30, Runs: 120,
		CostRatio: &costRatio, CostLower: &costLo, CostUpper: &costHi, BaselineRate: 0.55, CandidateRate: 0.67}
	fake := newFakeEvaluations(true,
		detail("running", nil, nil, 0, 0),
		detail("running", []evaluationCheckpoint{round1}, nil, 60, 36.2),
		detail("done", []evaluationCheckpoint{round1, round2}, verdict, 120, 72.4),
	)
	srv := httptest.NewServer(fake)
	defer srv.Close()

	var out strings.Builder
	// --yes does not skip the questions when the estimate needs confirmation.
	c := comparer{client: api.New(srv.URL, "cli"), in: bufio.NewReader(strings.NewReader("y\ny\n")), out: &out, interval: time.Millisecond}
	if err := c.run(context.Background(), testRequest(t), true); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"POST /api/v1/evaluations/estimate", "POST /api/v1/evaluations", "POST /api/v1/evaluations/01EVAL/confirmation",
		"GET /api/v1/evaluations/01EVAL", "GET /api/v1/evaluations/01EVAL", "GET /api/v1/evaluations/01EVAL",
	}
	if strings.Join(fake.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(fake.calls, "\n"))
	}
	sent := fake.bodies["/api/v1/evaluations"]
	if sent.Candidate.Model != "claude-opus-5" || sent.Baseline.Model != "claude-sonnet-5" || sent.Purpose != "compare" || sent.Repeats != 3 ||
		*sent.Prices["claude-opus-5"].Output != 25 {
		t.Fatalf("request = %+v", sent)
	}
	text := out.String()
	for _, line := range []string{
		"Change:       model claude-sonnet-5 → claude-opus-5",
		"30 approved dev cases × 2 sides × 3 repeats = 180 runs",
		"Baseline:     36,000,000 tokens, 40.50 USD",
		"Total:        108.00 USD (36.00 USD a round)",
		"Sandbox time: 42.0 hours",
		"Duration:     42 minutes to 42.0 hours",
		"Cap:          150.00 USD",
		"This month:   12.30 USD spent of the 500.00 USD monthly limit",
		"Confirmation: needed, because the total is above 50.00 USD",
		"Confirm the spend? [y/N]",
		"Round 1: Δ +8.0 points, 99% interval -2.0 to +18.0 points, 30 cases: inconclusive",
		"Round 2: Δ +12.0 points, 99% interval +4.0 to +20.0 points, 30 cases: better",
		"Verdict: better",
		"Rule: the 99% interval lies entirely above 0",
		"Pass rate: baseline 55.0%, candidate 67.0%, over 30 cases and 120 runs",
		"Cost per task, candidate ÷ baseline: 1.32×, 99% interval 1.18 to 1.47",
		"Spent 72.40 USD against an estimate of 108.00 USD",
		"A verdict is a controlled comparison",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("output lacks %q:\n%s", line, text)
		}
	}
	if strings.Count(text, "Round 1:") != 1 {
		t.Errorf("round 1 printed more than once:\n%s", text)
	}
}

func TestCompareStopsWhenThePersonSaysNo(t *testing.T) {
	fake := newFakeEvaluations(false)
	srv := httptest.NewServer(fake)
	defer srv.Close()
	var out strings.Builder
	c := comparer{client: api.New(srv.URL, "cli"), in: bufio.NewReader(strings.NewReader("\n")), out: &out, interval: time.Millisecond}
	if err := c.run(context.Background(), testRequest(t), false); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || !strings.Contains(out.String(), "Not started.") {
		t.Fatalf("calls %v, output:\n%s", fake.calls, out.String())
	}
}

func TestCtrlCDuringFollowAsksAndCancels(t *testing.T) {
	fake := newFakeEvaluations(false, detail("running", nil, nil, 0, 0))
	srv := httptest.NewServer(fake)
	defer srv.Close()
	interrupts := make(chan os.Signal, 1)
	interrupts <- os.Interrupt
	var out strings.Builder
	// --yes and no confirmation needed: the only question is the one Ctrl-C asks.
	c := comparer{client: api.New(srv.URL, "cli"), in: bufio.NewReader(strings.NewReader("y\n")), out: &out, interval: time.Hour, interrupts: interrupts}
	if err := c.run(context.Background(), testRequest(t), true); err != nil {
		t.Fatal(err)
	}
	if last := fake.calls[len(fake.calls)-1]; last != "POST /api/v1/evaluations/01EVAL/cancellation" {
		t.Fatalf("calls: %v", fake.calls)
	}
	if !strings.Contains(out.String(), "Cancel evaluation 01EVAL?") || !strings.Contains(out.String(), "is cancelled") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestCompareRefusesAnEstimateOverTheMonthlyLimit(t *testing.T) {
	fake := newFakeEvaluations(false)
	fake.estimate.MonthSpentUsd = 450
	srv := httptest.NewServer(fake)
	defer srv.Close()
	var out strings.Builder
	c := comparer{client: api.New(srv.URL, "cli"), in: bufio.NewReader(strings.NewReader("y\n")), out: &out, interval: time.Millisecond}
	err := c.run(context.Background(), testRequest(t), true)
	if err == nil || !strings.Contains(err.Error(), "over the monthly limit") || len(fake.calls) != 1 {
		t.Fatalf("err %v, calls %v", err, fake.calls)
	}
}

func TestTheCandidateChangesExactlyOneThing(t *testing.T) {
	high := "high"
	base := harnessSpec{Agent: "claude-code", AgentVersion: "2.4.1", Model: "claude-sonnet-5", Effort: &high, Harness: "main"}
	for _, tc := range []struct {
		change, refused string
		check           func(harnessSpec) bool
		purpose         string
	}{
		{change: "model=claude-opus-5", check: func(c harnessSpec) bool { return c.Model == "claude-opus-5" }, purpose: "compare"},
		{change: "effort=low", check: func(c harnessSpec) bool { return *c.Effort == "low" }, purpose: "compare"},
		{change: "harness=none", check: func(c harnessSpec) bool { return c.Harness == "none" }, purpose: "harness_vs_none"},
		{change: "harness=feature/rules", check: func(c harnessSpec) bool { return c.Harness == "feature/rules" }, purpose: "compare"},
		{change: "agent=claude-code@2.5.0", check: func(c harnessSpec) bool { return c.Agent == "claude-code" && c.AgentVersion == "2.5.0" }, purpose: "compare"},
		{change: "agent=codex@0.9.0", check: func(c harnessSpec) bool { return c.Agent == "codex" && c.AgentVersion == "0.9.0" }, purpose: "compare"},
		{change: "model=claude-opus-5,effort=low", refused: "more than one thing"},
		{change: "model=claude-sonnet-5", refused: "already uses"},
		{change: "effort=high", refused: "already uses"},
		{change: "harness=main", refused: "already uses"},
		{change: "agent=claude-code@2.4.1", refused: "already uses"},
		{change: "agent=claude-code", refused: "add @<version>"},
		{change: "agent=codex", refused: "needs a pinned version"},
		{change: "agent=command", refused: "command agent"},
		{change: "agent=aider@1", refused: "claude-code, codex, cursor-cli or command"},
		{change: "temperature=0", refused: "use one of"},
		{change: "model=", refused: "use one of"},
	} {
		c, purpose, err := applyChange(base, tc.change)
		if tc.refused != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("%s: err = %v, want %q", tc.change, err, tc.refused)
			}
			continue
		}
		if err != nil || !tc.check(c) || purpose != tc.purpose {
			t.Errorf("%s: candidate %+v, purpose %s, err %v", tc.change, c, purpose, err)
		}
		if *base.Effort != "high" {
			t.Fatalf("%s changed the baseline", tc.change)
		}
	}
}

func TestTheBaselineComesFromCaseboxYmlAndFlags(t *testing.T) {
	thirty := 30
	cfg := &repo.Baseline{Agent: "claude-code", AgentVersion: "2.4.1", Model: "claude-sonnet-5", TimeoutMinutes: &thirty}
	s, err := compareBaseline(cfg, "", "", func() string { return "trunk" })
	if err != nil || s.Harness != "trunk" || s.Model != "claude-sonnet-5" || *s.Settings.TimeoutMinutes != 30 || s.Effort != nil {
		t.Fatalf("baseline %+v, err %v", s, err)
	}
	s, err = compareBaseline(cfg, "", "agent=codex@0.9.0,model=gpt-6,harness=v2,effort=high", func() string { return "trunk" })
	if err != nil || s.Agent != "codex" || s.AgentVersion != "0.9.0" || s.Model != "gpt-6" || s.Harness != "v2" || *s.Effort != "high" {
		t.Fatalf("baseline %+v, err %v", s, err)
	}
	if _, err := compareBaseline(nil, "", "", func() string { return "main" }); err == nil || !strings.Contains(err.Error(), "evaluation.baseline") {
		t.Fatalf("err = %v", err)
	}
	if _, err := compareBaseline(nil, "", "agent=codex,model=gpt-6", func() string { return "main" }); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("err = %v", err)
	}
}
