// Package steering classifies human interventions in agent work with the team's analysis model,
// in the three steps of SDD section 6: intent, what went wrong, and what would have prevented it.
// The windows, examples, labels and result are the contract of docs/specs/steering.md.
package steering

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Label values, exactly as the spec names them.
var (
	Intents     = []string{"correction", "direction", "clarification", "routine"}
	WentWrongs  = []string{"missed_requirement", "broke_convention", "wrong_approach", "unverified_done", "wrong_area", "over_engineered", "lacked_domain_knowledge", "environment", "other"}
	Preventions = []string{"instruction", "skill", "tool_access", "verification", "clearer_ticket", "stronger_model", "nothing"}
	TaskTypes   = []string{"bug", "feature", "refactor", "test", "config", "docs"}
)

// Threshold is the confidence below which a label is not trusted: the intervention stays
// unclassified.
const Threshold = 0.6

// Turn is one agent turn next to an intervention.
type Turn struct {
	Text  string `json:"text,omitempty"`
	Tools []Tool `json:"tools"`
}

// Tool is one tool call of a turn.
type Tool struct {
	Name   string   `json:"name"`
	Status string   `json:"status,omitempty"`
	Files  []string `json:"files,omitempty"`
}

// Window is what the classifier may see of one intervention. A null in the server's JSON reads
// as the empty value.
type Window struct {
	InterventionID string   `json:"interventionId"`
	Signal         string   `json:"signal"`
	Phase          string   `json:"phase"`
	RuleIntent     string   `json:"ruleIntent,omitempty"`
	Agent          string   `json:"agent,omitempty"`
	Model          string   `json:"model,omitempty"`
	Human          string   `json:"human,omitempty"`
	Before         *Turn    `json:"before,omitempty"`
	After          *Turn    `json:"after,omitempty"`
	Files          []string `json:"files,omitempty"`
	Repo           string   `json:"repo,omitempty"`
	Commits        []string `json:"commits,omitempty"`
}

// Sendable reports whether the model may see the window at all. A window with no human text, no
// rule intent and no commits carries nothing to classify; the server records it as no_text.
func (w Window) Sendable() bool {
	return strings.TrimSpace(w.Human) != "" || w.RuleIntent != "" || len(w.Commits) > 0
}

// Task is the text a session's task type is read from.
type Task struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
}

// Example is an intervention a person of the team labelled.
type Example struct {
	Window         Window `json:"window"`
	Intent         string `json:"intent"`
	WentWrong      string `json:"wentWrong,omitempty"`
	WentWrongLabel string `json:"wentWrongLabel,omitempty"`
	Prevention     string `json:"prevention,omitempty"`
}

// Commit is a commit a before- or after-merge window names, read from the mirror.
type Commit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Diff    string `json:"diff"`
}

// Label is the classifier's answer for one intervention.
type Label struct {
	Intent         string  `json:"intent,omitempty"`
	WentWrong      string  `json:"wentWrong,omitempty"`
	WentWrongLabel string  `json:"wentWrongLabel,omitempty"`
	Prevention     string  `json:"prevention,omitempty"`
	Confidence     float64 `json:"confidence"`
}

// ErrInvalidOutput marks a model reply that does not match the label schema.
var ErrInvalidOutput = errors.New("invalid output")

// Validate checks the labels against the spec: known values only, what went wrong and prevention
// for corrections only, and a free label of at most 6 words with "other" only.
func (l Label) Validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidOutput, fmt.Sprintf(format, args...))
	}
	if !slices.Contains(Intents, l.Intent) {
		return invalid("intent %q is not one of %s", l.Intent, strings.Join(Intents, ", "))
	}
	if l.Confidence < 0 || l.Confidence > 1 {
		return invalid("confidence %v is outside 0 to 1", l.Confidence)
	}
	if l.Intent != "correction" {
		if l.WentWrong != "" || l.WentWrongLabel != "" || l.Prevention != "" {
			return invalid("a %s has no wentWrong, wentWrongLabel or prevention", l.Intent)
		}
		return nil
	}
	if !slices.Contains(WentWrongs, l.WentWrong) {
		return invalid("wentWrong %q is not one of %s", l.WentWrong, strings.Join(WentWrongs, ", "))
	}
	if !slices.Contains(Preventions, l.Prevention) {
		return invalid("prevention %q is not one of %s", l.Prevention, strings.Join(Preventions, ", "))
	}
	switch words := len(strings.Fields(l.WentWrongLabel)); {
	case l.WentWrong == "other" && words == 0:
		return invalid("wentWrong other needs a wentWrongLabel")
	case l.WentWrong != "other" && words > 0:
		return invalid("wentWrongLabel is only for wentWrong other")
	case words > 6:
		return invalid("wentWrongLabel has %d words; at most 6", words)
	}
	return nil
}
