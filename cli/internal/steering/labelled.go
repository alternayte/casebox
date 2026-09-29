package steering

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
)

//go:embed testdata/labelled.json
var labelledJSON []byte

// Labelled is one intervention of the labelled set. The set has no mirror, so a before- or
// after-merge window carries its commits' messages and diffs inline, one for each SHA the window
// names.
type Labelled struct {
	ID      string   `json:"id"`
	Window  Window   `json:"window"`
	Commits []Commit `json:"commits,omitempty"`
	Gold    Label    `json:"gold"`
	Note    string   `json:"note,omitempty"`
}

// LabelledSet returns the labelled set shipped in the CLI: synthetic transcripts only.
func LabelledSet() ([]Labelled, error) {
	var set struct {
		Examples []Labelled `json:"examples"`
	}
	if err := json.Unmarshal(labelledJSON, &set); err != nil {
		return nil, err
	}
	return set.Examples, nil
}

// Outcome is the model's answer for one labelled intervention.
type Outcome struct {
	Label Label
	Model string
	Err   error
}

// Check classifies each labelled intervention, at most concurrency at once, and pairs the answers
// with the gold labels. An invalid answer, a failed call and an answer below the threshold are
// unclassified. When every call failed, it returns the first error: the model is not reachable.
func (c *Classifier) Check(ctx context.Context, set []Labelled, concurrency int) ([]Pair, []Outcome, error) {
	outcomes := make([]Outcome, len(set))
	Each(ctx, len(set), concurrency, func(ctx context.Context, i int) {
		label, model, err := c.Classify(ctx, set[i].Window, set[i].Commits)
		outcomes[i] = Outcome{Label: label, Model: model, Err: err}
	})
	if err := ctx.Err(); err != nil {
		return nil, outcomes, err
	}
	pairs := make([]Pair, len(set))
	failed := 0
	var firstErr error
	for i, o := range outcomes {
		pairs[i] = Pair{Gold: set[i].Gold, Model: o.Label, Classified: o.Err == nil && o.Label.Confidence >= Threshold}
		if o.Err != nil && !errors.Is(o.Err, ErrInvalidOutput) {
			failed++
			if firstErr == nil {
				firstErr = o.Err
			}
		}
	}
	if len(set) > 0 && failed == len(set) {
		return nil, outcomes, firstErr
	}
	return pairs, outcomes, nil
}
