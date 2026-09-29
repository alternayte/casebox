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

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/gitmirror"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/steering"
	"github.com/alternayte/casebox/cli/internal/worker"
	"go.yaml.in/yaml/v3"
)

// Jobs runs propose.search and propose.diet.
type Jobs struct {
	Client      *api.Client
	MirrorRoot  string
	GitHubToken string
	Model       *analysis.Client // nil: no search, only the diet
}

type payload struct {
	Pattern   string   `json:"pattern"`
	Proposals []string `json:"proposals"`
	Repo      string   `json:"repo"`
}

// Candidate is a drafted candidate: its edits, the blob of its files, and why.
type Candidate struct {
	Edits     []Edit `json:"edits"`
	Overrides string `json:"overrides"`
	Rationale string `json:"rationale"`
}

// Answer is the job's result.
type Answer struct {
	BaseCommit string      `json:"baseCommit"`
	Candidates []Candidate `json:"candidates"`
}

// Overrides is the blob a candidate's harness side reads: its changed files, null to remove.
type Overrides struct {
	Repo  string             `json:"repo"`
	Files map[string]*string `json:"files"`
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
	Windows []steering.Window `json:"windows"`
	Cases   []struct {
		ID          string  `json:"id"`
		Instruction *string `json:"instruction"`
	} `json:"cases"`
	Rejections []struct {
		Edits  json.RawMessage `json:"edits"`
		Reason *string         `json:"reason"`
	} `json:"rejections"`
}

// harness reads the repository's harness at its default branch from the worker's mirror.
func (j Jobs) harness(ctx context.Context, name string) (string, map[string]string, func(string) bool, error) {
	m, err := gitmirror.Open(ctx, j.MirrorRoot, name, j.GitHubToken)
	if err != nil {
		return "", nil, nil, err
	}
	commit, err := repo.Resolve(ctx, m.Dir, "HEAD")
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve the default branch of %s: %w", name, err)
	}
	var cfg repo.Config
	if raw, err := repo.ReadAt(ctx, m.Dir, commit, repo.ConfigPath); err == nil {
		_ = yaml.Unmarshal(raw, &cfg)
	}
	globs := cfg.HarnessGlobs()
	found, err := repo.Harness(ctx, m.Dir, commit, globs)
	if err != nil {
		return "", nil, nil, err
	}
	files := map[string]string{}
	for _, f := range found.Files {
		body, err := repo.ReadAt(ctx, m.Dir, commit, f)
		if err != nil {
			return "", nil, nil, fmt.Errorf("read %s: %w", f, err)
		}
		files[f] = string(body)
	}
	return commit, files, func(f string) bool { return repo.MatchesAny(globs, f) }, nil
}

const searchPrompt = `You improve a team's coding-agent harness: its AGENTS.md or CLAUDE.md instructions and its skills.
A recurring correction shows what the agent keeps getting wrong. Propose 3 to 5 different candidate
changes that would prevent it. Each candidate makes 1 to 3 small edits:
- add_bullet: add one bullet under a heading of a file (the heading is created when missing);
- replace_bullet: make one existing bullet (old, its exact text) specific (new);
- delete_bullet or delete_section: remove a bullet or a "##" section that misleads the agent;
- write_skill: create or change .claude/skills/<name>/SKILL.md (or .agents/skills/<name>/SKILL.md) with a
  short procedure; delete_skill removes one.
Never rewrite a whole file. Keep each bullet to one sentence, concrete and checkable. Do not repeat a
rejected change. Give each candidate one sentence on why it prevents the correction.
"[person]" marks a person; never guess who anyone is.`

// Search drafts candidates for one pattern with the analysis model.
func (j Jobs) Search(ctx context.Context, job worker.Job) (any, error) {
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
	commit, files, matches, err := j.harness(ctx, p.Repo)
	if err != nil {
		return nil, err
	}

	var user strings.Builder
	fmt.Fprintf(&user, "The recurring correction: %s\n%s\nWhat went wrong: %s", ev.Pattern.Title, ev.Pattern.Summary, ev.Pattern.WentWrong)
	if ev.Pattern.Label != nil {
		fmt.Fprintf(&user, " (%s)", *ev.Pattern.Label)
	}
	fmt.Fprintf(&user, "\nWhat would have prevented it: %s\nArea of the repository: %s\n\n", ev.Pattern.Prevention, ev.Pattern.Path)
	user.WriteString("Corrections (the human's words and the agent's turn before):\n\n")
	for _, w := range ev.Windows {
		if w.Human != "" {
			fmt.Fprintf(&user, "- human: %s\n", clip(w.Human, 600))
		}
		if w.Before != nil && w.Before.Text != "" {
			fmt.Fprintf(&user, "  agent before: %s\n", clip(w.Before.Text, 400))
		}
	}
	if len(ev.Cases) > 0 {
		user.WriteString("\nTasks where this went wrong:\n")
		for _, c := range ev.Cases {
			if c.Instruction != nil {
				fmt.Fprintf(&user, "- %s\n", clip(*c.Instruction, 400))
			}
		}
	}
	if len(ev.Rejections) > 0 {
		user.WriteString("\nRejected before; do not propose these again:\n")
		for _, r := range ev.Rejections {
			reason := ""
			if r.Reason != nil {
				reason = " (reason: " + *r.Reason + ")"
			}
			fmt.Fprintf(&user, "- %s%s\n", clip(string(r.Edits), 600), reason)
		}
	}
	user.WriteString("\nThe harness files now:\n")
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		fmt.Fprintf(&user, "\n=== %s ===\n%s\n", f, clip(files[f], 12000))
	}

	res, err := j.Model.Complete(ctx, analysis.Request{System: searchPrompt, User: clip(user.String(), 90000), Tool: "candidates", Schema: candidateSchema(), MaxTokens: 6000})
	if err != nil {
		return nil, err
	}
	var drafted struct {
		Candidates []struct {
			Rationale string `json:"rationale"`
			Edits     []Edit `json:"edits"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(res.JSON, &drafted); err != nil {
		return nil, fmt.Errorf("the model's candidates are not valid: %w", err)
	}
	answer := Answer{BaseCommit: commit}
	var refused []string
	for _, c := range drafted.Candidates {
		changed, err := Apply(files, c.Edits, matches)
		if err != nil {
			refused = append(refused, err.Error())
			continue
		}
		hash, err := j.put(ctx, p.Repo, changed)
		if err != nil {
			return nil, err
		}
		answer.Candidates = append(answer.Candidates, Candidate{Edits: c.Edits, Overrides: hash, Rationale: clip(c.Rationale, 400)})
		if len(answer.Candidates) == 5 {
			break
		}
	}
	if len(answer.Candidates) == 0 {
		return nil, worker.Permanent{Err: fmt.Errorf("the model proposed no valid edit: %s", strings.Join(refused, "; "))}
	}
	return answer, nil
}

// Diet lists removal candidates: each "##" section of AGENTS.md and CLAUDE.md and each skill, largest
// first, at most one per proposal the job names.
func (j Jobs) Diet(ctx context.Context, job worker.Job) (any, error) {
	var p payload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.Repo == "" || len(p.Proposals) == 0 {
		return nil, worker.Permanent{Err: errors.New("the job names no repository or proposals")}
	}
	commit, files, matches, err := j.harness(ctx, p.Repo)
	if err != nil {
		return nil, err
	}
	var parts []Part
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		for _, s := range Sections(files[f]) {
			s.File = f
			parts = append(parts, s)
		}
	}
	for f, body := range files {
		if skillFile.MatchString(f) {
			parts = append(parts, Part{File: f, Size: len(body)})
		}
	}
	sort.SliceStable(parts, func(a, b int) bool {
		if parts[a].Size != parts[b].Size {
			return parts[a].Size > parts[b].Size
		}
		return parts[a].File+parts[a].Heading < parts[b].File+parts[b].Heading
	})
	answer := Answer{BaseCommit: commit}
	for _, part := range parts {
		if len(answer.Candidates) == len(p.Proposals) {
			break
		}
		edit := Edit{Op: "delete_section", File: part.File, Heading: part.Heading}
		why := fmt.Sprintf("Removes the section %q of %s (%d bytes the agent reads on every turn).", part.Heading, part.File, part.Size)
		if part.Heading == "" {
			edit = Edit{Op: "delete_skill", File: part.File}
			why = fmt.Sprintf("Removes the skill %s (%d bytes).", skillName(part.File), part.Size)
		}
		changed, err := Apply(files, []Edit{edit}, matches)
		if err != nil {
			continue
		}
		hash, err := j.put(ctx, p.Repo, changed)
		if err != nil {
			return nil, err
		}
		answer.Candidates = append(answer.Candidates, Candidate{Edits: []Edit{edit}, Overrides: hash, Rationale: why})
	}
	return answer, nil
}

func skillName(skill string) string {
	parts := strings.Split(skill, "/")
	if len(parts) >= 3 {
		return parts[len(parts)-2]
	}
	return skill
}

func (j Jobs) put(ctx context.Context, repoName string, changed map[string]*string) (string, error) {
	data, err := json.Marshal(Overrides{Repo: repoName, Files: changed})
	if err != nil {
		return "", err
	}
	return j.Client.PutBlob(ctx, "application/json", data)
}

func candidateSchema() map[string]any {
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
		"required": []string{"candidates"},
		"properties": map[string]any{
			"candidates": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"rationale", "edits"},
					"properties": map[string]any{
						"rationale": map[string]any{"type": "string"},
						"edits":     map[string]any{"type": "array", "items": edit},
					},
				},
			},
		},
	}
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "…"
}
