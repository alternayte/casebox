package steering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/alternayte/casebox/cli/internal/analysis"
)

// Classifier labels windows with one analysis model, guided by the team's examples.
type Classifier struct {
	Model    *analysis.Client
	Examples []Example
}

// reply is the model's answer; a null reads as the empty value.
type reply struct {
	Reason         string   `json:"reason"`
	Intent         *string  `json:"intent"`
	WentWrong      *string  `json:"wentWrong"`
	WentWrongLabel *string  `json:"wentWrongLabel"`
	Prevention     *string  `json:"prevention"`
	Confidence     *float64 `json:"confidence"`
}

// Classify labels one window. For a window with a rule intent it asks only what went wrong and
// what would have prevented it; a rule intent other than correction needs no model call. It returns
// the model ID the provider reported. An error wrapping ErrInvalidOutput is a reply that does not
// match the schema; any other error is a failed call.
func (c *Classifier) Classify(ctx context.Context, w Window, commits []Commit) (Label, string, error) {
	if !w.Sendable() {
		return Label{}, "", errors.New("the window has no text, no rule intent and no commits")
	}
	if w.RuleIntent != "" && w.RuleIntent != "correction" {
		label := Label{Intent: w.RuleIntent, Confidence: 1}
		return label, "", label.Validate()
	}
	res, err := c.Model.Complete(ctx, analysis.Request{
		System: system(c.Examples),
		User:   user(w, commits),
		Tool:   "label",
		Schema: labelSchema(w.RuleIntent == ""),
	})
	var invalid *analysis.InvalidOutputError
	if errors.As(err, &invalid) {
		return Label{}, "", fmt.Errorf("%w: %s", ErrInvalidOutput, invalid.Reason)
	}
	if err != nil {
		return Label{}, "", err
	}
	var r reply
	if err := json.Unmarshal(res.JSON, &r); err != nil {
		return Label{}, res.Model, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	if r.Confidence == nil {
		return Label{}, res.Model, fmt.Errorf("%w: no confidence", ErrInvalidOutput)
	}
	label := Label{
		Intent:         str(r.Intent),
		WentWrong:      str(r.WentWrong),
		WentWrongLabel: strings.TrimSpace(str(r.WentWrongLabel)),
		Prevention:     str(r.Prevention),
		Confidence:     *r.Confidence,
	}
	if w.RuleIntent != "" {
		label.Intent = w.RuleIntent
	}
	return label, res.Model, label.Validate()
}

// ClassifyTask names the task type of a session's task text. It returns "" when the model is
// unsure (below the threshold).
func (c *Classifier) ClassifyTask(ctx context.Context, text string) (string, string, error) {
	res, err := c.Model.Complete(ctx, analysis.Request{
		System:    taskPrompt,
		User:      "The task:\n" + clip(text, 4000),
		Tool:      "task_type",
		Schema:    taskSchema(),
		MaxTokens: 200,
	})
	if err != nil {
		return "", "", err
	}
	var r struct {
		TaskType   string  `json:"taskType"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(res.JSON, &r); err != nil || !slices.Contains(TaskTypes, r.TaskType) {
		return "", res.Model, fmt.Errorf("%w: task type %q", ErrInvalidOutput, r.TaskType)
	}
	if r.Confidence < Threshold {
		return "", res.Model, nil
	}
	return r.TaskType, res.Model, nil
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// Each runs fn for every index below n, with at most limit at once.
func Each(ctx context.Context, n, limit int, fn func(ctx context.Context, i int)) {
	sem := make(chan struct{}, max(limit, 1))
	var wg sync.WaitGroup
	for i := range n {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(ctx, i)
		}()
	}
	wg.Wait()
}
