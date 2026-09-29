package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/steering"
)

// clusterPayload is a pattern.cluster job: one group of corrections of a workspace
// (docs/specs/self-evolution.md, Patterns).
type clusterPayload struct {
	Workspace string `json:"workspace"`
	Key       struct {
		WentWrong  string  `json:"wentWrong"`
		Label      *string `json:"label"`
		Prevention string  `json:"prevention"`
		Path       string  `json:"path"`
	} `json:"key"`
	Corrections []struct {
		Ref            string `json:"ref"`
		Stream         string `json:"stream"`
		InterventionID string `json:"interventionId"`
	} `json:"corrections"`
}

// ClusterPart is one part of a group that shares one cause.
type ClusterPart struct {
	Label   string   `json:"label"`
	Summary string   `json:"summary"`
	Refs    []string `json:"refs"`
}

type clusterResult struct {
	Model string        `json:"model"`
	Parts []ClusterPart `json:"parts"`
}

const clusterPrompt = `You group corrections a team made to its coding agents. Every correction below already shares
what went wrong, what would have prevented it, and the area of the repository. Split them into parts
that share one specific cause, so that one small change to the agent's instructions or skills could
prevent every correction in a part. A correction that shares a cause with no other belongs to no part.
Give each part a label of at most 8 words and one sentence that says what the agent keeps doing wrong.
Refer to corrections only by their ref. Never guess who anyone is; "[person]" marks a person.`

// Cluster splits one group of corrections with this host's analysis model.
func (s Steering) Cluster(ctx context.Context, job Job) (any, error) {
	if s.Model == nil {
		return nil, Permanent{errors.New("this worker has no analysis model")}
	}
	var p clusterPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || len(p.Corrections) == 0 {
		return nil, Permanent{errors.New("the job names no corrections")}
	}
	byStream := map[string][]string{}
	refOf := map[string]string{}
	for _, c := range p.Corrections {
		byStream[c.Stream] = append(byStream[c.Stream], c.InterventionID)
		refOf[c.Stream+"\x00"+c.InterventionID] = c.Ref
	}
	var text strings.Builder
	for stream, ids := range byStream {
		var windows struct {
			Windows []steering.Window `json:"windows"`
		}
		if err := s.Client.Do(ctx, http.MethodPost, "/worker/v1/steering/windows", map[string]any{"stream": stream, "interventionIds": ids}, &windows); err != nil {
			return nil, err
		}
		for _, w := range windows.Windows {
			ref := refOf[stream+"\x00"+w.InterventionID]
			if ref == "" {
				continue
			}
			fmt.Fprintf(&text, "ref: %s\nsignal: %s (%s)\n", ref, w.Signal, w.Phase)
			if w.Human != "" {
				fmt.Fprintf(&text, "human: %s\n", clipText(w.Human, 800))
			}
			if w.Before != nil && w.Before.Text != "" {
				fmt.Fprintf(&text, "agent before: %s\n", clipText(w.Before.Text, 500))
			}
			if len(w.Files) > 0 {
				fmt.Fprintf(&text, "files: %s\n", strings.Join(w.Files, ", "))
			}
			text.WriteString("\n")
		}
	}
	label := p.Key.WentWrong
	if p.Key.Label != nil {
		label += " (" + *p.Key.Label + ")"
	}
	user := fmt.Sprintf("What went wrong: %s\nPrevention: %s\nArea: %s\n\nCorrections:\n\n%s", label, p.Key.Prevention, p.Key.Path, clipText(text.String(), 60000))
	res, err := s.Model.Complete(ctx, analysis.Request{System: clusterPrompt, User: user, Tool: "parts", Schema: clusterSchema(), MaxTokens: 4000})
	if err != nil {
		return nil, err
	}
	var r struct {
		Parts []ClusterPart `json:"parts"`
	}
	if err := json.Unmarshal(res.JSON, &r); err != nil {
		return nil, fmt.Errorf("the model's parts are not valid: %w", err)
	}
	return clusterResult{Model: res.Model, Parts: r.Parts}, nil
}

func clusterSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"parts"},
		"properties": map[string]any{
			"parts": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"label", "summary", "refs"},
					"properties": map[string]any{
						"label":   map[string]any{"type": "string"},
						"summary": map[string]any{"type": "string"},
						"refs":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
			},
		},
	}
}

func clipText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "…"
}
