package cases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// Candidate is one case the server proposes for mining.
type Candidate struct {
	Key        string      `json:"key"`
	Kind       string      `json:"kind"`
	WorkItem   *string     `json:"workItem"`
	Rank       int         `json:"rank"`
	Pulls      []Pull      `json:"pulls"`
	Original   *Pull       `json:"original"`
	Related    []Pull      `json:"related"`
	Session    *string     `json:"session"`
	Base       *string     `json:"base"`
	Correction *Correction `json:"correction"`
}

// MinePayload is the payload of a mine job: one workspace repository and its candidates.
type MinePayload struct {
	Workspace      string      `json:"workspace"`
	Repo           string      `json:"repo"`
	RecipeHash     string      `json:"recipeHash"`
	WindowDays     int         `json:"windowDays"`
	MaxSourceFiles int         `json:"maxSourceFiles"`
	Candidates     []Candidate `json:"candidates"`
}

// MinedCase is a candidate that passed the filters.
type MinedCase struct {
	Key         string     `json:"key"`
	Kind        string     `json:"kind"`
	Scope       string     `json:"scope"`
	Repos       []CaseRepo `json:"repos"`
	HarnessHash string     `json:"harnessHash"`
	Rank        int        `json:"rank"`
	TestFiles   int        `json:"testFiles"`
	SourceFiles int        `json:"sourceFiles"`
}

// Skipped is a candidate the filters dropped, with why.
type Skipped struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// MineResult is the answer to a mine job.
type MineResult struct {
	Cases   []MinedCase `json:"cases"`
	Skipped []Skipped   `json:"skipped"`
}

// Skip reasons.
const (
	SkipUnknownKind       = "unknown_kind"
	SkipOtherRepo         = "other_repository"
	SkipNoPull            = "no_pull"
	SkipBot               = "bot"
	SkipOutsideWindow     = "outside_window"
	SkipNotDefaultBranch  = "not_default_branch"
	SkipNotMerged         = "not_merged"
	SkipOriginalNotMerged = "original_not_merged"
	SkipNoTestChange      = "no_test_change"
	SkipNoSourceChange    = "no_source_change"
	SkipTooManySources    = "too_many_source_files"
	SkipBaseUnknown       = "base_unknown"
)

// Mine runs a mine job: it reads the repository's mirror and answers which candidates are cases.
func (j Jobs) Mine(ctx context.Context, job worker.Job) (any, error) {
	var p MinePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.Repo == "" {
		return nil, worker.Permanent{Err: errors.New("the job names no repository")}
	}
	return mine(ctx, j.mirrors(), p, j.now())
}

// miner holds what every candidate of one repository is checked against.
type miner struct {
	open      source
	dir       string
	branch    string
	cfg       repo.Config
	since     time.Time
	maxSource int
}

func mine(ctx context.Context, open source, p MinePayload, now time.Time) (MineResult, error) {
	dir, err := open(ctx, p.Repo)
	if err != nil {
		return MineResult{}, err
	}
	branch, err := defaultBranch(ctx, dir)
	if err != nil {
		return MineResult{}, err
	}
	cfg := config(ctx, dir)
	window := firstPositive(cfg.Cases.Window, p.WindowDays, repo.DefaultCaseWindowDays)
	m := miner{
		open:      open,
		dir:       dir,
		branch:    branch,
		cfg:       cfg,
		since:     now.AddDate(0, 0, -window),
		maxSource: firstPositive(cfg.Cases.MaxSourceFiles, p.MaxSourceFiles, repo.DefaultMaxSourceFiles),
	}
	res := MineResult{Cases: []MinedCase{}, Skipped: []Skipped{}}
	for _, c := range p.Candidates {
		mined, reason, err := m.candidate(ctx, p.Repo, c)
		if err != nil {
			return MineResult{}, fmt.Errorf("candidate %s: %w", c.Key, err)
		}
		if reason != "" {
			res.Skipped = append(res.Skipped, Skipped{Key: c.Key, Reason: reason})
			continue
		}
		res.Cases = append(res.Cases, mined)
	}
	return res, nil
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

// candidate applies the filters of its kind and decides its scope. A non-empty reason skips it.
func (m miner) candidate(ctx context.Context, name string, c Candidate) (MinedCase, string, error) {
	var (
		base, merged string
		counted      []string // files the filters count: the fix's for a regression
	)
	switch c.Kind {
	case Capability, Regression:
		if len(c.Pulls) == 0 {
			return MinedCase{}, SkipNoPull, nil
		}
		pull := c.Pulls[0]
		if reason := m.merged(ctx, name, pull); reason != "" {
			return MinedCase{}, reason, nil
		}
		if c.Kind == Regression {
			// The original may be older than the window or written by a bot; only the fix is the case.
			if c.Original == nil || m.onDefaultBranch(ctx, name, *c.Original) != "" {
				return MinedCase{}, SkipOriginalNotMerged, nil
			}
		}
		var err error
		if base, err = firstParent(ctx, m.dir, pull.MergeSha); err != nil {
			return MinedCase{}, SkipNotMerged, nil
		}
		merged = pull.MergeSha
		if counted, err = changedFiles(ctx, m.dir, base, merged); err != nil {
			return MinedCase{}, "", err
		}
	case Steering:
		if c.Base == nil || !hasCommit(ctx, m.dir, *c.Base) {
			return MinedCase{}, SkipBaseUnknown, nil
		}
		base = *c.Base
		// The work item's latest pull request merged into the default branch gives the tests.
		var latest *Pull
		for i, pull := range c.Pulls {
			if m.merged(ctx, name, pull) == "" && (latest == nil || pull.MergedAt.After(latest.MergedAt)) {
				latest = &c.Pulls[i]
			}
		}
		if latest != nil {
			merged = latest.MergeSha
		}
	default:
		return MinedCase{}, SkipUnknownKind, nil
	}

	tests, sources := 0, 0
	for _, f := range counted {
		if repo.IsTestFile(m.cfg.TestGlobs(), f) {
			tests++
		} else {
			sources++
		}
	}
	if c.Kind != Steering {
		switch {
		case tests == 0:
			return MinedCase{}, SkipNoTestChange, nil
		case sources == 0:
			return MinedCase{}, SkipNoSourceChange, nil
		case sources > m.maxSource:
			return MinedCase{}, SkipTooManySources, nil
		}
	}

	sealed := CaseRepo{Repo: name, Base: base, Role: RoleSealed}
	if merged != "" {
		sealed.Merged = ptr(merged)
	}
	scope, others, err := m.scope(ctx, name, c.Related)
	if err != nil {
		return MinedCase{}, "", err
	}
	h, err := repo.Harness(ctx, m.dir, base, m.cfg.HarnessGlobs())
	if err != nil {
		return MinedCase{}, "", err
	}
	return MinedCase{
		Key:         c.Key,
		Kind:        c.Kind,
		Scope:       scope,
		Repos:       append([]CaseRepo{sealed}, others...),
		HarnessHash: h.Hash,
		Rank:        c.Rank,
		TestFiles:   tests,
		SourceFiles: sources,
	}, "", nil
}

// merged checks that a pull request was merged into the default branch within the window, by a
// person, and that the mirror has its merge commit on that branch.
func (m miner) merged(ctx context.Context, name string, p Pull) string {
	switch {
	case p.Bot:
		return SkipBot
	case p.MergedAt.Before(m.since):
		return SkipOutsideWindow
	}
	return m.onDefaultBranch(ctx, name, p)
}

// onDefaultBranch checks that a pull request of this repository targeted the default branch and
// that its merge commit is on it.
func (m miner) onDefaultBranch(ctx context.Context, name string, p Pull) string {
	switch {
	case p.Repo != "" && p.Repo != name:
		return SkipOtherRepo
	case p.BaseRef != m.branch:
		return SkipNotDefaultBranch
	case !hasCommit(ctx, m.dir, p.MergeSha):
		return SkipNotMerged
	}
	if _, err := git(ctx, m.dir, "merge-base", "--is-ancestor", p.MergeSha, "refs/heads/"+m.branch); err != nil {
		return SkipNotMerged
	}
	return ""
}

// scope decides single, split or multi from the work item's pull requests in the workspace's
// other repositories. Multi needs the recipe's links; otherwise the others are context, present at
// their latest merge.
func (m miner) scope(ctx context.Context, name string, related []Pull) (string, []CaseRepo, error) {
	latest := map[string]Pull{}
	for _, p := range related {
		if p.Repo == "" || p.Repo == name || p.MergeSha == "" {
			continue
		}
		if cur, ok := latest[p.Repo]; !ok || p.MergedAt.After(cur.MergedAt) {
			latest[p.Repo] = p
		}
	}
	if len(latest) == 0 {
		return ScopeSingle, nil, nil
	}
	names := make([]string, 0, len(latest))
	for r := range latest {
		names = append(names, r)
	}
	sort.Strings(names)
	multi := m.cfg.Environment != nil && len(m.cfg.Environment.Links) > 0
	var out []CaseRepo
	for _, r := range names {
		p := latest[r]
		if !multi {
			out = append(out, CaseRepo{Repo: r, Base: p.MergeSha, Role: RoleContext})
			continue
		}
		dir, err := m.open(ctx, r)
		if err != nil {
			return "", nil, fmt.Errorf("open %s: %w", r, err)
		}
		if !hasCommit(ctx, dir, p.MergeSha) {
			return "", nil, fmt.Errorf("%s has no commit %s of pull request #%d", r, short(p.MergeSha), p.Number)
		}
		base, err := firstParent(ctx, dir, p.MergeSha)
		if err != nil {
			return "", nil, err
		}
		out = append(out, CaseRepo{Repo: r, Base: base, Merged: ptr(p.MergeSha), Role: RoleSealed})
	}
	if multi {
		return ScopeMulti, out, nil
	}
	return ScopeSplit, out, nil
}
