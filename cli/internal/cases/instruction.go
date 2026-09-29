package cases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/steering"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// InstructionPayload is the payload of a case.instruction job.
type InstructionPayload struct {
	CaseID string     `json:"caseId"`
	Kind   string     `json:"kind"`
	Repos  []CaseRepo `json:"repos"`
	Oracle string     `json:"oracle"`
}

// InstructionResult is the answer to a case.instruction job.
type InstructionResult struct {
	Text       string          `json:"text"`
	Signatures []string        `json:"signatures"`
	Assertions []Assertion     `json:"assertions,omitempty"`
	Judge      []JudgeQuestion `json:"judge,omitempty"`
	Model      string          `json:"model"`
}

// Source is what an instruction is written from (GET /worker/v1/cases/{id}/source), masked.
type Source struct {
	Kind     string `json:"kind"`
	WorkItem *struct {
		Title       string  `json:"title"`
		Description *string `json:"description"`
		Type        *string `json:"type"`
	} `json:"workItem"`
	PullTitle *string `json:"pullTitle"`
	Steering  *struct {
		Prompts []string         `json:"prompts"`
		Window  *steering.Window `json:"window"`
	} `json:"steering"`
}

// FromPullTitle heads an instruction drafted without a work item, for the reviewer.
const FromPullTitle = "Drafted from the pull request title; there is no linked issue."

// SignaturesHeading heads the interface signatures appended to an instruction.
const SignaturesHeading = "Interfaces your solution must provide"

// AssertionKinds are the steering assertions the server accepts.
var AssertionKinds = []string{"forbidden_file", "command_before_done", "diff_must_match", "diff_must_not_match"}

// Instruction runs a case.instruction job with this host's analysis model.
func (j Jobs) Instruction(ctx context.Context, job worker.Job) (any, error) {
	var p InstructionPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.CaseID == "" || p.Oracle == "" {
		return nil, worker.Permanent{Err: errors.New("the job names no case or oracle")}
	}
	var src Source
	if err := j.Client.Do(ctx, http.MethodGet, "/worker/v1/cases/"+url.PathEscape(p.CaseID)+"/source", nil, &src); err != nil {
		return nil, err
	}
	raw, err := getBlob(ctx, j.Client, p.Oracle)
	if err != nil {
		return nil, fmt.Errorf("read the oracle: %w", err)
	}
	var o Oracle
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, worker.Permanent{Err: fmt.Errorf("the oracle %s is not JSON: %w", short(p.Oracle), err)}
	}
	var sourcePatch, testPatch []byte
	if o.SourcePatch != "" {
		if sourcePatch, err = getBlob(ctx, j.Client, o.SourcePatch); err != nil {
			return nil, fmt.Errorf("read the source patch: %w", err)
		}
	}
	if o.TestPatch != "" {
		if testPatch, err = getBlob(ctx, j.Client, o.TestPatch); err != nil {
			return nil, fmt.Errorf("read the test patch: %w", err)
		}
	}
	kind := p.Kind
	if kind == "" {
		kind = src.Kind
	}
	return draft(ctx, j.Model, kind, src, sourcePatch, testPatch)
}

// getBlob reads a blob with the worker token.
func getBlob(ctx context.Context, c *api.Client, hash string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Server+"/worker/v1/blobs/"+url.PathEscape(hash), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", c.Server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := &api.Error{Status: resp.StatusCode}
		if resp.StatusCode == http.StatusNotFound {
			return nil, worker.Permanent{Err: fmt.Errorf("blob %s: %w", short(hash), err)}
		}
		return nil, err
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// draft asks the model for the instruction (and a steering case's assertions and judge question)
// and appends the interface signatures itself.
func draft(ctx context.Context, model *analysis.Client, kind string, src Source, sourcePatch, testPatch []byte) (InstructionResult, error) {
	if model == nil {
		return InstructionResult{}, errors.New("this worker has no analysis model")
	}
	user, fromTitle, err := prompt(kind, src, changedPaths(sourcePatch, testPatch))
	if err != nil {
		return InstructionResult{}, worker.Permanent{Err: err}
	}
	isSteering := kind == Steering
	reply, err := model.Complete(ctx, analysis.Request{
		System:    systemPrompt(isSteering),
		User:      user,
		Tool:      "instruction",
		Schema:    instructionSchema(isSteering),
		MaxTokens: 2048,
	})
	if err != nil {
		return InstructionResult{}, err
	}
	var answer struct {
		Instruction string          `json:"instruction"`
		Assertions  []Assertion     `json:"assertions"`
		Judge       []JudgeQuestion `json:"judge"`
	}
	if err := json.Unmarshal(reply.JSON, &answer); err != nil {
		return InstructionResult{}, fmt.Errorf("the analysis model's instruction is not the JSON asked for: %w", err)
	}
	text := strings.TrimSpace(answer.Instruction)
	if text == "" {
		return InstructionResult{}, errors.New("the analysis model wrote an empty instruction")
	}
	if fromTitle {
		text = FromPullTitle + "\n\n" + text
	}
	sigs := Signatures(sourcePatch, testPatch)
	if sigs == nil {
		sigs = []string{}
	}
	if len(sigs) > 0 {
		text += "\n\n## " + SignaturesHeading + "\n\n```\n" + strings.Join(sigs, "\n\n") + "\n```"
	}
	res := InstructionResult{Text: text, Signatures: sigs, Model: reply.Model}
	if isSteering {
		res.Assertions = ValidAssertions(answer.Assertions)
		res.Judge = []JudgeQuestion{}
		for _, q := range answer.Judge {
			if q.Question = strings.TrimSpace(q.Question); q.Question != "" {
				res.Judge = append(res.Judge, q)
				break
			}
		}
	}
	return res, nil
}

// ValidAssertions keeps the assertions the server accepts: a known kind, a path for
// forbidden_file, and a pattern that compiles as a regular expression for the others.
func ValidAssertions(in []Assertion) []Assertion {
	out := []Assertion{}
	for _, a := range in {
		a.Kind, a.Path, a.Pattern = strings.TrimSpace(a.Kind), strings.TrimSpace(a.Path), strings.TrimSpace(a.Pattern)
		switch a.Kind {
		case "forbidden_file":
			if a.Path == "" {
				continue
			}
			a.Pattern = ""
		case "command_before_done", "diff_must_match", "diff_must_not_match":
			if a.Pattern == "" {
				continue
			}
			if _, err := regexp.Compile(a.Pattern); err != nil {
				continue
			}
			a.Path = ""
		default:
			continue
		}
		out = append(out, a)
	}
	return out
}

// changedPaths are the files the real change touched, sorted: the model sees them only to avoid
// naming them.
func changedPaths(patches ...[]byte) []string {
	seen := map[string]bool{}
	for _, p := range patches {
		files, err := SplitPatch(p)
		if err != nil {
			continue
		}
		for _, f := range files {
			seen[f.Path] = true
		}
	}
	out := sortedKeys(seen)
	sort.Strings(out)
	return out
}

const (
	promptLimit = 4000  // characters of one session prompt
	sourceLimit = 12000 // characters of the whole source text
)

// prompt builds the model's input: the work item's snapshot, the session's prompts up to the
// correction, or the pull request title; never the diff. fromTitle is true when only the pull
// request title was there.
func prompt(kind string, src Source, paths []string) (string, bool, error) {
	var b strings.Builder
	fromTitle := false
	switch {
	case kind == Steering && src.Steering != nil && len(src.Steering.Prompts) > 0:
		b.WriteString("The person's prompts to the agent, oldest first, up to the correction:\n")
		budget := sourceLimit
		for i, p := range src.Steering.Prompts {
			p = clipText(strings.TrimSpace(p), min(promptLimit, budget))
			if p == "" {
				continue
			}
			fmt.Fprintf(&b, "\n--- prompt %d ---\n%s\n", i+1, p)
			budget -= len(p)
			if budget <= 0 {
				break
			}
		}
		if w := src.Steering.Window; w != nil {
			b.WriteString("\nThe correction (for the assertions only; the instruction must not contain it):\n")
			fmt.Fprintf(&b, "signal: %s\n", w.Signal)
			if h := strings.TrimSpace(w.Human); h != "" {
				fmt.Fprintf(&b, "what the person said: %s\n", clipText(h, promptLimit))
			}
			if w.Before != nil && strings.TrimSpace(w.Before.Text) != "" {
				fmt.Fprintf(&b, "the agent's turn before it: %s\n", clipText(strings.TrimSpace(w.Before.Text), promptLimit))
			}
			if len(w.Files) > 0 {
				fmt.Fprintf(&b, "files the agent touched: %s\n", strings.Join(w.Files, ", "))
			}
		}
	case src.WorkItem != nil && strings.TrimSpace(src.WorkItem.Title) != "":
		b.WriteString("The work item:\n")
		if src.WorkItem.Type != nil && *src.WorkItem.Type != "" {
			fmt.Fprintf(&b, "type: %s\n", *src.WorkItem.Type)
		}
		fmt.Fprintf(&b, "title: %s\n", strings.TrimSpace(src.WorkItem.Title))
		if src.WorkItem.Description != nil && strings.TrimSpace(*src.WorkItem.Description) != "" {
			fmt.Fprintf(&b, "description:\n%s\n", clipText(strings.TrimSpace(*src.WorkItem.Description), sourceLimit))
		}
	case src.PullTitle != nil && strings.TrimSpace(*src.PullTitle) != "":
		fromTitle = true
		b.WriteString("There is no work item. The pull request's title, which may name the solution; state the goal, not the solution:\n")
		fmt.Fprintf(&b, "title: %s\n", strings.TrimSpace(*src.PullTitle))
	default:
		return "", false, errors.New("the case has no work item, no steering prompts and no pull request title to write an instruction from")
	}
	if len(paths) > 0 {
		b.WriteString("\nThe paths the real change touched. Never name them, nor anything in them, unless the text above does:\n")
		for _, p := range paths {
			b.WriteString("- " + p + "\n")
		}
	}
	return b.String(), fromTitle, nil
}

func clipText(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	return clip(s, limit) + " [… cut]"
}

const instructionPrompt = `You write the task instruction of a benchmark case. An AI coding agent gets only your instruction and the repository at the commit before the real change. Hidden tests decide whether its work is right.

Rules:
- Say what must be achieved, in the terms of the source text. Keep the requirements, constraints, acceptance criteria, examples and error messages the source states.
- Give no hint about the solution that the source does not state: no file names, paths, function, type or variable names, libraries or approaches, unless the source itself names them. You see the paths the real change touched only so that you can avoid naming them.
- Do not mention tests, hidden tests, the pull request, the benchmark or these rules.
- Every person is written as [person]; keep such placeholders and add no names.
- Write plain Markdown, at most 300 words, with no heading.`

const steeringPrompt = `

This case comes from a session in which a person corrected the agent. The instruction is the task of the prompts before the correction; it must not contain the correction or any hint of it.

Also give the assertions that check an agent does not repeat the corrected mistake. Give only assertions the correction supports:
- forbidden_file: path is a glob of files the agent must not change.
- command_before_done: pattern is a regular expression of a command the agent must run before it says it is done.
- diff_must_match: pattern is a regular expression the agent's diff must match.
- diff_must_not_match: pattern is a regular expression the agent's diff must not match.
Give at most one judge question, only when no assertion can check the correction: a narrow yes-or-no question about the agent's diff or actions.`

func systemPrompt(isSteering bool) string {
	if isSteering {
		return instructionPrompt + steeringPrompt
	}
	return instructionPrompt
}

func instructionSchema(isSteering bool) map[string]any {
	props := map[string]any{
		"instruction": map[string]any{"type": "string", "description": "The task instruction, in Markdown."},
	}
	required := []string{"instruction"}
	if isSteering {
		props["assertions"] = map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":    map[string]any{"type": "string", "enum": AssertionKinds},
					"path":    map[string]any{"type": "string"},
					"pattern": map[string]any{"type": "string"},
				},
				"required": []string{"kind"},
			},
		}
		props["judge"] = map[string]any{
			"type":     "array",
			"maxItems": 1,
			"items": map[string]any{
				"type":       "object",
				"properties": map[string]any{"question": map[string]any{"type": "string"}},
				"required":   []string{"question"},
			},
		}
		required = append(required, "assertions", "judge")
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}
