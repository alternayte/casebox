package steering

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alternayte/casebox/cli/internal/analysis"
)

func TestKappaMatchesKnownValues(t *testing.T) {
	repeat := func(label string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = label
		}
		return out
	}
	// The textbook 2×2 table: yes/yes 20, yes/no 5, no/yes 10, no/no 15. po = 0.7, pe = 0.5.
	a := append(append(append(repeat("yes", 20), repeat("yes", 5)...), repeat("no", 10)...), repeat("no", 15)...)
	b := append(append(append(repeat("yes", 20), repeat("no", 5)...), repeat("yes", 10)...), repeat("no", 15)...)
	for _, c := range []struct {
		name            string
		a, b            []string
		agreement, want float64
	}{
		{"textbook", a, b, 0.7, 0.4},
		{"chance", []string{"x", "x", "y", "y"}, []string{"x", "y", "x", "y"}, 0.5, 0},
		{"perfect", []string{"x", "y", "z"}, []string{"x", "y", "z"}, 1, 1},
		{"one label each side", []string{"x", "x"}, []string{"x", "x"}, 1, 1},
		{"worse than chance", []string{"x", "y"}, []string{"y", "x"}, 0, -1},
	} {
		got := Kappa(c.a, c.b)
		if math.Abs(got.Agreement-c.agreement) > 1e-9 || math.Abs(got.Kappa-c.want) > 1e-9 || got.N != len(c.a) {
			t.Errorf("%s: got %+v, want agreement %v kappa %v", c.name, got, c.agreement, c.want)
		}
	}
}

func TestAgreementScoresWhatWentWrongOnlyWhereBothSayCorrection(t *testing.T) {
	corr := func(w, p string) Label {
		return Label{Intent: "correction", WentWrong: w, Prevention: p, Confidence: 0.9}
	}
	pairs := []Pair{
		{Gold: corr("wrong_area", "instruction"), Model: corr("wrong_area", "instruction"), Classified: true},
		{Gold: corr("unverified_done", "verification"), Model: corr("wrong_approach", "verification"), Classified: true},
		{Gold: corr("environment", "nothing"), Model: Label{Intent: "direction", Confidence: 0.8}, Classified: true},
		{Gold: Label{Intent: "routine"}, Model: Label{Intent: "routine", Confidence: 0.4}, Classified: false},
	}
	a := Agree(pairs)
	if a.Total != 4 || a.Unclassified != 1 || a.UnclassifiedShare() != 0.25 {
		t.Fatalf("totals = %+v", a)
	}
	if a.Intent.N != 3 || math.Abs(a.Intent.Agreement-2.0/3) > 1e-9 {
		t.Fatalf("intent = %+v", a.Intent)
	}
	if a.WentWrong.N != 2 || a.WentWrong.Agreement != 0.5 || a.Prevention.Agreement != 1 {
		t.Fatalf("wentWrong = %+v, prevention = %+v", a.WentWrong, a.Prevention)
	}
	if a.Confusion["correction"]["direction"] != 1 || a.Confusion["correction"]["correction"] != 2 {
		t.Fatalf("confusion = %v", a.Confusion)
	}
}

func TestLabelsAreValidatedStrictly(t *testing.T) {
	for _, c := range []struct {
		name  string
		label Label
		ok    bool
	}{
		{"correction", Label{Intent: "correction", WentWrong: "wrong_area", Prevention: "instruction", Confidence: 0.8}, true},
		{"direction", Label{Intent: "direction", Confidence: 0.7}, true},
		{"other with label", Label{Intent: "correction", WentWrong: "other", WentWrongLabel: "launch date moved", Prevention: "nothing", Confidence: 0.7}, true},
		{"unknown intent", Label{Intent: "complaint", Confidence: 0.9}, false},
		{"unknown class", Label{Intent: "correction", WentWrong: "bad_code", Prevention: "instruction", Confidence: 0.9}, false},
		{"correction without prevention", Label{Intent: "correction", WentWrong: "wrong_area", Confidence: 0.9}, false},
		{"direction with a class", Label{Intent: "direction", WentWrong: "wrong_area", Confidence: 0.9}, false},
		{"label without other", Label{Intent: "correction", WentWrong: "wrong_area", WentWrongLabel: "x", Prevention: "skill", Confidence: 0.9}, false},
		{"other without label", Label{Intent: "correction", WentWrong: "other", Prevention: "skill", Confidence: 0.9}, false},
		{"label of seven words", Label{Intent: "correction", WentWrong: "other", WentWrongLabel: "one two three four five six seven", Prevention: "skill", Confidence: 0.9}, false},
		{"confidence above 1", Label{Intent: "routine", Confidence: 1.5}, false},
	} {
		err := c.label.Validate()
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalidOutput)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// The labelled set must stay a fair test: every gold label valid, every window one the worker
// would send, every commit's text present, and every class of every step at least twice.
func TestTheLabelledSetCoversEveryClass(t *testing.T) {
	set, err := LabelledSet()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) < 60 {
		t.Fatalf("%d examples, want at least 60", len(set))
	}
	counts := map[string]int{}
	ids := map[string]bool{}
	for _, ex := range set {
		if ids[ex.ID] {
			t.Errorf("duplicate id %s", ex.ID)
		}
		ids[ex.ID] = true
		if err := ex.Gold.Validate(); err != nil {
			t.Errorf("%s: gold %v", ex.ID, err)
		}
		if !ex.Window.Sendable() {
			t.Errorf("%s: the window would never be sent to the model", ex.ID)
		}
		if len(ex.Commits) != len(ex.Window.Commits) {
			t.Errorf("%s: %d commit texts for %d commits", ex.ID, len(ex.Commits), len(ex.Window.Commits))
		}
		for i, c := range ex.Commits {
			if c.SHA != ex.Window.Commits[i] || c.Message == "" || c.Diff == "" {
				t.Errorf("%s: commit %d is incomplete", ex.ID, i)
			}
		}
		if ex.Window.RuleIntent != "" && ex.Gold.Intent != ex.Window.RuleIntent {
			t.Errorf("%s: gold intent %s differs from the rule intent", ex.ID, ex.Gold.Intent)
		}
		counts["signal:"+ex.Window.Signal]++
		counts["intent:"+ex.Gold.Intent]++
		counts["wentWrong:"+ex.Gold.WentWrong]++
		counts["prevention:"+ex.Gold.Prevention]++
		if ex.Window.Human == "" && ex.Window.Before != nil && ex.Window.Before.Text == "" {
			counts["structure only"]++
		}
	}
	var want []string
	for _, s := range []string{"follow_up", "interruption", "denial", "rewind", "human_edit", "restarted", "human_rewrite", "review_change", "ci_fix", "revert", "fix"} {
		want = append(want, "signal:"+s)
	}
	for _, v := range Intents {
		want = append(want, "intent:"+v)
	}
	for _, v := range WentWrongs {
		want = append(want, "wentWrong:"+v)
	}
	for _, v := range Preventions {
		want = append(want, "prevention:"+v)
	}
	want = append(want, "structure only")
	for _, k := range want {
		if counts[k] < 2 {
			t.Errorf("%s: %d examples, want at least 2", k, counts[k])
		}
	}
}

// fakeAnthropic answers every call with the tool input the test gives, and records the requests.
func fakeAnthropic(t *testing.T, answer string, seen *[]map[string]any) *Classifier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		*seen = append(*seen, body)
		_, _ = w.Write([]byte(`{"model":"claude-test-1","content":[{"type":"tool_use","name":"label","input":` + answer + `}]}`))
	}))
	t.Cleanup(srv.Close)
	return &Classifier{Model: analysis.New(analysis.Config{Provider: analysis.Anthropic, Model: "claude-test", BaseURL: srv.URL, APIKey: "k"})}
}

// A window with a rule intent asks only what went wrong and what would have prevented it, and the
// commits' messages and diffs reach the model.
func TestARuleIntentWindowAsksOnlyForTheCauseAndPrevention(t *testing.T) {
	var seen []map[string]any
	c := fakeAnthropic(t, `{"reason":"r","intent":"routine","wentWrong":"environment","wentWrongLabel":null,"prevention":"nothing","confidence":0.8}`, &seen)
	w := Window{InterventionID: "c:1", Signal: "ci_fix", Phase: "before_merge", RuleIntent: "correction", Commits: []string{"abc"}}
	label, model, err := c.Classify(context.Background(), w, []Commit{{SHA: "abc", Message: "ci: pin the linter", Diff: "-version: latest\n+version: v1.61.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if label.Intent != "correction" || label.WentWrong != "environment" || model != "claude-test-1" {
		t.Fatalf("label = %+v, model %s", label, model)
	}
	props := seen[0]["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)["properties"].(map[string]any)
	if _, asked := props["intent"]; asked {
		t.Fatal("the schema asks for the intent of a rule-intent window")
	}
	msg := seen[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(msg, "ci: pin the linter") || !strings.Contains(msg, "+version: v1.61.0") {
		t.Fatalf("the prompt lacks the commit:\n%s", msg)
	}
}

func TestAnAnswerOutsideTheSchemaIsInvalidOutput(t *testing.T) {
	var seen []map[string]any
	c := fakeAnthropic(t, `{"reason":"r","intent":"direction","wentWrong":"wrong_area","prevention":null,"confidence":0.9}`, &seen)
	_, _, err := c.Classify(context.Background(), Window{Signal: "follow_up", Phase: "in_session", Human: "also add a metric"}, nil)
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("err = %v, want invalid output", err)
	}
	if _, _, err := c.Classify(context.Background(), Window{Signal: "interruption", Phase: "in_session"}, nil); err == nil || len(seen) != 1 {
		t.Fatalf("a window with nothing to classify reached the model (err %v, %d calls)", err, len(seen))
	}
}
