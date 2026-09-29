package propose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/gitmirror"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/steering"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// Jobs runs proposal.draft on a worker with an analysis model.
type Jobs struct {
	Client     *api.Client
	MirrorRoot string
	Remotes    *gitmirror.Remotes
	Model      *analysis.Client
}

// Preview is a changed file as the draft saw it: Before is nil for a new file.
type Preview struct {
	Path   string  `json:"path"`
	Before *string `json:"before"`
	After  string  `json:"after"`
}

// Note is what a code note asks the person's agent to do.
type Note struct {
	What   string `json:"what"`
	Why    string `json:"why"`
	Prompt string `json:"prompt"`
}

// Answer is the job's result: a draft, or an advisory note when no repository change helps.
type Answer struct {
	Advisory   string    `json:"advisory,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Title      string    `json:"title,omitempty"`
	Rationale  string    `json:"rationale,omitempty"`
	Edits      []Edit    `json:"edits,omitempty"`
	Preview    []Preview `json:"preview,omitempty"`
	Note       *Note     `json:"note,omitempty"`
	BaseCommit string    `json:"baseCommit,omitempty"`
	Model      string    `json:"model,omitempty"`
}

type payload struct {
	Pattern string `json:"pattern"`
	Repo    string `json:"repo"`
}

type evidence struct {
	Pattern struct {
		Title      string  `json:"title"`
		Summary    string  `json:"summary"`
		WentWrong  string  `json:"wentWrong"`
		Label      *string `json:"label"`
		Prevention string  `json:"prevention"`
		Path       string  `json:"path"`
	} `json:"pattern"`
	Windows    []steering.Window `json:"windows"`
	Rejections []struct {
		Title  string          `json:"title"`
		Edits  json.RawMessage `json:"edits"`
		Note   json.RawMessage `json:"note"`
		Reason *string         `json:"reason"`
	} `json:"rejections"`
}

const draftPrompt = `You help a team's coding agent get its work right the first time. A recurring correction
shows what the agent keeps getting wrong in this repository. Propose ONE small change that would
have prevented it, of exactly one kind:
- harness_edit: 1 to 3 edits of AGENTS.md, CLAUDE.md or a .cursor/rules/*.mdc file: add_bullet (heading,
  and new: one sentence), replace_bullet (old: the exact bullet, new: its sharper text) or delete_bullet
  (old: a bullet that misleads the agent).
- skill: one write_skill (.claude/skills/<name>/SKILL.md with YAML front matter "name" and "description",
  then a short numbered procedure) or one write_rule (.cursor/rules/<name>.mdc with front matter
  "description" and "globs" or "alwaysApply", then the rule).
- mcp: one add_mcp_server to .cursor/mcp.json or .mcp.json: heading is the server's name, new is its JSON
  ({"command", "args"} or {"url"}); every secret is written as ${VARIABLE}. Only when the corrections
  show the agent lacked a tool or live information.
- code_note: when the code makes the mistake easy (no single test command, no check, a missing helper):
  "what" to change, "why", and a "prompt" a coding agent can carry out in this repository.
- advisory: when no repository change prevents it (an unclear ticket, a weak model, missing access):
  one sentence on why, and nothing else.
Prefer the smallest change that would have prevented most of the corrections. Keep each bullet to one
concrete, checkable sentence. Never rewrite a whole file. Do not propose a change like a rejected one;
read the reasons. The title says what changes in under 10 words. "[person]" marks a person; never
guess who anyone is.`

// Draft drafts one proposal for a pattern from its corrections and the repository's harness files
// on the default branch.
func (j Jobs) Draft(ctx context.Context, job worker.Job) (any, error) {
	if j.Model == nil {
		return nil, worker.Permanent{Err: errors.New("this worker has no analysis model")}
	}
	var p payload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.Pattern == "" || p.Repo == "" {
		return nil, worker.Permanent{Err: errors.New("the job names no pattern or repository")}
	}
	var ev evidence
	if err := j.Client.Do(ctx, http.MethodGet, "/worker/v1/patterns/"+url.PathEscape(p.Pattern)+"/evidence", nil, &ev); err != nil {
		return nil, err
	}
	commit, files, matches, read, err := j.harness(ctx, p.Repo)
	if err != nil {
		return nil, err
	}

	res, err := j.Model.Complete(ctx, analysis.Request{System: draftPrompt, User: clip(prompt(ev, files), 90000), Tool: "proposal", Schema: draftSchema(), MaxTokens: 6000})
	if err != nil {
		return nil, err
	}
	var d struct {
		Kind      string `json:"kind"`
		Title     string `json:"title"`
		Rationale string `json:"rationale"`
		Edits     []Edit `json:"edits"`
		Note      *Note  `json:"note"`
		Advisory  string `json:"advisory"`
	}
	if err := json.Unmarshal(res.JSON, &d); err != nil {
		return nil, fmt.Errorf("the model's proposal is not valid: %w", err)
	}
	answer := Answer{Kind: d.Kind, Title: clip(d.Title, 200), Rationale: clip(d.Rationale, 1000), BaseCommit: commit, Model: res.Model}
	switch d.Kind {
	case "advisory":
		if strings.TrimSpace(d.Advisory) == "" {
			return nil, worker.Permanent{Err: errors.New("the model found no change but gave no reason")}
		}
		return Answer{Advisory: clip(d.Advisory, 600), Model: res.Model}, nil
	case "code_note":
		if d.Note == nil || strings.TrimSpace(d.Note.Prompt) == "" || strings.TrimSpace(d.Note.What) == "" {
			return nil, worker.Permanent{Err: errors.New("the model's code note has no what or prompt")}
		}
		answer.Note = &Note{What: clip(d.Note.What, 1000), Why: clip(d.Note.Why, 1000), Prompt: clip(d.Note.Prompt, 4000)}
		return answer, nil
	case "harness_edit", "skill", "mcp":
	default:
		return nil, worker.Permanent{Err: fmt.Errorf("the model proposed the kind %q", d.Kind)}
	}
	if err := kindMatches(d.Kind, d.Edits); err != nil {
		return nil, worker.Permanent{Err: err}
	}
	for _, e := range d.Edits {
		if _, ok := files[e.File]; !ok {
			if body, ok := read(e.File); ok {
				files[e.File] = body
			}
		}
	}
	changed, err := Apply(files, d.Edits, func(f string) bool { return matches(f) || whole(f) })
	if err != nil {
		return nil, worker.Permanent{Err: fmt.Errorf("the model's edits do not apply: %w", err)}
	}
	answer.Edits = d.Edits
	names := make([]string, 0, len(changed))
	for name := range changed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		after := ""
		if changed[name] != nil {
			after = *changed[name]
		}
		pv := Preview{Path: name, After: after}
		if before, ok := files[name]; ok {
			b := before
			pv.Before = &b
		}
		answer.Preview = append(answer.Preview, pv)
	}
	return answer, nil
}

// kindMatches checks that a draft's edits are the ones its kind allows.
func kindMatches(kind string, edits []Edit) error {
	allowed := map[string][]string{
		"harness_edit": {"add_bullet", "replace_bullet", "delete_bullet"},
		"skill":        {"write_skill", "write_rule"},
		"mcp":          {"add_mcp_server"},
	}[kind]
	if len(edits) == 0 {
		return fmt.Errorf("a %s proposal makes at least one edit", kind)
	}
	for _, e := range edits {
		ok := false
		for _, op := range allowed {
			ok = ok || e.Op == op
		}
		if !ok {
			return fmt.Errorf("a %s proposal does not use %s", kind, e.Op)
		}
	}
	return nil
}

func prompt(ev evidence, files map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The recurring correction: %s\n%s\nWhat went wrong: %s", ev.Pattern.Title, ev.Pattern.Summary, ev.Pattern.WentWrong)
	if ev.Pattern.Label != nil {
		fmt.Fprintf(&b, " (%s)", *ev.Pattern.Label)
	}
	fmt.Fprintf(&b, "\nWhat the classifier says would have prevented it: %s\nArea of the repository: %s\n\n", ev.Pattern.Prevention, ev.Pattern.Path)
	b.WriteString("Corrections (the human's words and the agent's turn before):\n\n")
	for _, w := range ev.Windows {
		if w.Human != "" {
			fmt.Fprintf(&b, "- human: %s\n", clip(w.Human, 600))
		}
		if w.Before != nil && w.Before.Text != "" {
			fmt.Fprintf(&b, "  agent before: %s\n", clip(w.Before.Text, 400))
		}
	}
	if len(ev.Rejections) > 0 {
		b.WriteString("\nRejected before; do not propose these again:\n")
		for _, r := range ev.Rejections {
			reason := ""
			if r.Reason != nil {
				reason = " (reason: " + *r.Reason + ")"
			}
			fmt.Fprintf(&b, "- %s: %s %s%s\n", r.Title, clip(string(r.Edits), 600), clip(string(r.Note), 400), reason)
		}
	}
	b.WriteString("\nThe harness files now:\n")
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		fmt.Fprintf(&b, "\n=== %s ===\n%s\n", f, clip(files[f], 12000))
	}
	return b.String()
}

// harness reads the repository's harness files at its default branch from the worker's mirror,
// and returns a reader for other files at the same commit.
func (j Jobs) harness(ctx context.Context, name string) (string, map[string]string, func(string) bool, func(string) (string, bool), error) {
	m, err := gitmirror.Open(ctx, j.MirrorRoot, name, j.Remotes)
	if err != nil {
		return "", nil, nil, nil, err
	}
	commit, err := repo.Resolve(ctx, m.Dir, "HEAD")
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("resolve the default branch of %s: %w", name, err)
	}
	var cfg repo.Config
	if raw, err := repo.ReadAt(ctx, m.Dir, commit, repo.ConfigPath); err == nil {
		_ = yaml.Unmarshal(raw, &cfg)
	}
	globs := cfg.HarnessGlobs()
	found, err := repo.Harness(ctx, m.Dir, commit, globs)
	if err != nil {
		return "", nil, nil, nil, err
	}
	files := map[string]string{}
	for _, f := range found.Files {
		body, err := repo.ReadAt(ctx, m.Dir, commit, f)
		if err != nil {
			return "", nil, nil, nil, fmt.Errorf("read %s: %w", f, err)
		}
		files[f] = string(body)
	}
	read := func(f string) (string, bool) {
		body, err := repo.ReadAt(ctx, m.Dir, commit, f)
		return string(body), err == nil
	}
	return commit, files, func(f string) bool { return repo.MatchesAny(globs, f) }, read, nil
}

func draftSchema() map[string]any {
	edit := map[string]any{
		"type":     "object",
		"required": []string{"op", "file"},
		"properties": map[string]any{
			"op":      map[string]any{"type": "string", "enum": Ops},
			"file":    map[string]any{"type": "string"},
			"heading": map[string]any{"type": "string"},
			"old":     map[string]any{"type": "string"},
			"new":     map[string]any{"type": "string"},
		},
	}
	return map[string]any{
		"type":     "object",
		"required": []string{"kind", "title", "rationale"},
		"properties": map[string]any{
			"kind":      map[string]any{"type": "string", "enum": []string{"harness_edit", "skill", "mcp", "code_note", "advisory"}},
			"title":     map[string]any{"type": "string"},
			"rationale": map[string]any{"type": "string"},
			"edits":     map[string]any{"type": "array", "items": edit},
			"note": map[string]any{
				"type":       "object",
				"properties": map[string]any{"what": map[string]any{"type": "string"}, "why": map[string]any{"type": "string"}, "prompt": map[string]any{"type": "string"}},
			},
			"advisory": map[string]any{"type": "string"},
		},
	}
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "…"
}
