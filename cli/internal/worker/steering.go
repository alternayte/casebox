package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/gitmirror"
	"github.com/alternayte/casebox/cli/internal/steering"
)

// Steering runs the steering jobs of docs/specs/steering.md: classifying interventions with this
// host's analysis model, and reading pull requests for human rewrites and review changes.
type Steering struct {
	Client      *api.Client
	MirrorRoot  string
	GitHubToken string
	Model       *analysis.Client // nil when this host has no analysis model
	Concurrency int
}

type classifyPayload struct {
	Stream          string   `json:"stream"`
	InterventionIDs []string `json:"interventionIds"`
	TaskFor         string   `json:"taskFor,omitempty"`
}

type classifyResult struct {
	Model         string               `json:"model"`
	PromptVersion string               `json:"promptVersion"`
	Results       []interventionResult `json:"results"`
	TaskType      string               `json:"taskType,omitempty"`
}

type interventionResult struct {
	InterventionID string  `json:"interventionId"`
	Intent         string  `json:"intent,omitempty"`
	WentWrong      string  `json:"wentWrong,omitempty"`
	WentWrongLabel string  `json:"wentWrongLabel,omitempty"`
	Prevention     string  `json:"prevention,omitempty"`
	Confidence     float64 `json:"confidence"`
	Error          string  `json:"error,omitempty"`
}

// Classify labels up to 20 interventions of one stream. A window with nothing to classify gets no
// result; the server records it as no_text. A failed call for one window becomes that window's
// error (model_error); only when the call failed for every window does the job fail, so an outage
// is retried instead of being recorded.
func (s Steering) Classify(ctx context.Context, job Job) (any, error) {
	var p classifyPayload
	// A job names interventions, the session whose task type is unknown, or both.
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.Stream == "" || (len(p.InterventionIDs) == 0 && p.TaskFor == "") {
		return nil, Permanent{errors.New("the job names no interventions and no task")}
	}
	var windows struct {
		Windows []steering.Window `json:"windows"`
		Task    *steering.Task    `json:"task"`
	}
	if err := s.Client.Do(ctx, http.MethodPost, "/worker/v1/steering/windows", p, &windows); err != nil {
		return nil, err
	}
	var examples struct {
		Examples []steering.Example `json:"examples"`
	}
	if err := s.Client.Do(ctx, http.MethodGet, "/worker/v1/steering/examples", nil, &examples); err != nil {
		return nil, err
	}
	commits, err := s.commitTexts(ctx, windows.Windows)
	if err != nil {
		return nil, err
	}

	classifier := steering.Classifier{Model: s.Model, Examples: examples.Examples}
	var send []steering.Window
	for _, w := range windows.Windows {
		if w.Sendable() {
			send = append(send, w)
		}
	}
	results := make([]interventionResult, len(send))
	models := make([]string, len(send))
	failed := make([]error, len(send))
	steering.Each(ctx, len(send), s.Concurrency, func(ctx context.Context, i int) {
		w := send[i]
		var texts []steering.Commit
		for _, sha := range w.Commits {
			if c, ok := commits[sha]; ok {
				texts = append(texts, c)
			}
		}
		label, model, err := classifier.Classify(ctx, w, texts)
		models[i] = model
		r := interventionResult{InterventionID: w.InterventionID}
		if err != nil && !errors.Is(err, steering.ErrInvalidOutput) {
			r.Error = err.Error()
			failed[i] = err
		} else {
			// An invalid answer goes to the server as the model gave it; the server's own
			// validation records it as invalid_output.
			r.Intent, r.WentWrong, r.WentWrongLabel, r.Prevention, r.Confidence = label.Intent, label.WentWrong, label.WentWrongLabel, label.Prevention, label.Confidence
		}
		results[i] = r
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(send) > 0 && allFailed(failed) {
		return nil, fmt.Errorf("the analysis model failed for every window: %w", failed[0])
	}

	out := classifyResult{Model: s.Model.Model, PromptVersion: steering.PromptVersion, Results: results}
	for _, m := range models {
		if m != "" {
			out.Model = m
			break
		}
	}
	if windows.Task != nil && strings.TrimSpace(windows.Task.Text) != "" {
		// The task type is a nice-to-have next to the labels; a failure leaves it unknown.
		if taskType, _, err := classifier.ClassifyTask(ctx, windows.Task.Text); err == nil {
			out.TaskType = taskType
		}
	}
	return out, nil
}

func allFailed(errs []error) bool {
	for _, err := range errs {
		if err == nil {
			return false
		}
	}
	return true
}

// commitTexts reads the message and diff of every commit the windows name from the repository's
// mirror, at most 8 000 characters of diff per window (the prompt cuts the rest).
func (s Steering) commitTexts(ctx context.Context, windows []steering.Window) (map[string]steering.Commit, error) {
	out := map[string]steering.Commit{}
	mirrors := map[string]*gitmirror.Mirror{}
	for _, w := range windows {
		if len(w.Commits) == 0 || !w.Sendable() || w.Repo == "" {
			continue
		}
		m, ok := mirrors[w.Repo]
		if !ok {
			var err error
			if m, err = gitmirror.Open(ctx, s.MirrorRoot, w.Repo, s.GitHubToken); err != nil {
				return nil, err
			}
			mirrors[w.Repo] = m
		}
		budget := 8000
		for _, sha := range w.Commits {
			if _, done := out[sha]; done {
				continue
			}
			c, ok := readCommit(ctx, m, sha, budget)
			if !ok {
				continue // gone from the remote, such as after a force push; the prompt says so
			}
			budget -= len(c.Diff)
			out[sha] = c
		}
	}
	return out, nil
}

func readCommit(ctx context.Context, m *gitmirror.Mirror, sha string, budget int) (steering.Commit, bool) {
	msg, err := m.Git(ctx, "log", "-1", "--format=%B", sha, "--")
	if err != nil {
		return steering.Commit{}, false
	}
	c := steering.Commit{SHA: sha, Message: strings.TrimSpace(string(msg))}
	if budget > 0 {
		if diff, err := m.Git(ctx, "show", "--no-color", "--no-ext-diff", "--format=", "--diff-merges=first-parent", sha, "--"); err == nil {
			c.Diff = string(diff)
			if len(c.Diff) > budget {
				c.Diff = c.Diff[:budget] + "\n[… cut]"
			}
		}
	}
	return c, true
}

type prPayload struct {
	Repo     string      `json:"repo"`
	Number   int         `json:"number"`
	BaseSHA  string      `json:"baseSha"`
	HeadSHA  string      `json:"headSha"`
	Commits  []prCommit  `json:"commits"`
	Comments []prComment `json:"comments"`
}

type prCommit struct {
	SHA   string    `json:"sha"`
	Agent bool      `json:"agent"`
	At    time.Time `json:"at"`
}

type prComment struct {
	ID        json.RawMessage `json:"id"`
	Path      string          `json:"path"`
	Line      int             `json:"line"`
	CommitSHA string          `json:"commitSha"`
	At        time.Time       `json:"at"`
}

type rewrite struct {
	SHA   string   `json:"sha"`
	Lines int      `json:"lines"`
	Files []string `json:"files"`
}

type reviewChange struct {
	CommentID json.RawMessage `json:"commentId"`
	SHA       string          `json:"sha"`
}

// PR reads one pull request from the mirror: which human commits rewrote lines of its agent
// commits, and which review comments a later commit answered by changing the commented lines.
func (s Steering) PR(ctx context.Context, job Job) (any, error) {
	var p prPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.Repo == "" || p.Number <= 0 {
		return nil, Permanent{errors.New("the job names no repository or pull request")}
	}
	m, err := gitmirror.Open(ctx, s.MirrorRoot, p.Repo, s.GitHubToken)
	if err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("refs/pull/%d/head", p.Number)
	if err := m.Fetch(ctx, "+"+ref+":"+ref); err != nil {
		if _, err2 := m.Git(ctx, "cat-file", "-e", p.HeadSHA+"^{commit}"); p.HeadSHA == "" || err2 != nil {
			return nil, fmt.Errorf("fetch %s of %s: %w", ref, p.Repo, err)
		}
	}
	rw, err := rewrites(ctx, m, p.Commits)
	if err != nil {
		return nil, err
	}
	rc := reviewChanges(ctx, m, p.Commits, p.Comments)
	return map[string]any{"rewrites": rw, "reviewChanges": rc}, nil
}

// rewrites blames, for each human commit after the first agent commit, the lines it removes or
// changes in its parent, and counts those an agent commit of the pull request wrote.
func rewrites(ctx context.Context, m *gitmirror.Mirror, commits []prCommit) ([]rewrite, error) {
	ordered := slicesSortedByTime(commits)
	agent := map[string]bool{}
	first := -1
	for i, c := range ordered {
		if c.Agent {
			agent[c.SHA] = true
			if first < 0 {
				first = i
			}
		}
	}
	out := []rewrite{}
	if first < 0 {
		return out, nil
	}
	for _, c := range ordered[first+1:] {
		if c.Agent {
			continue
		}
		parents, err := m.Git(ctx, "rev-list", "--parents", "-n", "1", c.SHA, "--")
		if err != nil {
			return nil, fmt.Errorf("read commit %s: %w", c.SHA, err)
		}
		if len(strings.Fields(string(parents))) != 2 {
			continue // a merge or a root commit rewrites nothing by itself
		}
		diff, err := m.Git(ctx, "-c", "core.quotePath=false", "diff", "-U0", "--no-color", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/", "-M", c.SHA+"^", c.SHA, "--")
		if err != nil {
			return nil, fmt.Errorf("diff %s: %w", c.SHA, err)
		}
		lines, files := 0, []string{}
		for path, ranges := range oldRanges(string(diff), false) {
			n, err := blameAgentLines(ctx, m, c.SHA+"^", path, ranges, agent)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				lines += n
				files = append(files, path)
			}
		}
		if lines > 0 {
			sort.Strings(files)
			out = append(out, rewrite{SHA: c.SHA, Lines: lines, Files: files})
		}
	}
	return out, nil
}

// blameAgentLines counts the lines of path at rev, within ranges, that an agent commit wrote.
func blameAgentLines(ctx context.Context, m *gitmirror.Mirror, rev, path string, ranges [][2]int, agent map[string]bool) (int, error) {
	args := []string{"blame", "--porcelain"}
	for _, r := range ranges {
		args = append(args, "-L", fmt.Sprintf("%d,%d", r[0], r[1]))
	}
	out, err := m.Git(ctx, append(args, rev, "--", path)...)
	if err != nil {
		return 0, fmt.Errorf("blame %s at %s: %w", path, rev, err)
	}
	count := 0
	current := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "\t") {
			if agent[current] {
				count++
			}
			continue
		}
		if f := strings.Fields(line); len(f) >= 3 && len(f[0]) >= 40 && isHex(f[0]) {
			current = f[0]
		}
	}
	return count, nil
}

// reviewChanges finds, for each review comment on a line, the first commit after it whose diff
// from the comment's commit changes a line within 3 lines of the commented one.
func reviewChanges(ctx context.Context, m *gitmirror.Mirror, commits []prCommit, comments []prComment) []reviewChange {
	ordered := slicesSortedByTime(commits)
	out := []reviewChange{}
	for _, cm := range comments {
		if cm.Path == "" || cm.Line <= 0 || cm.CommitSHA == "" {
			continue
		}
		for _, c := range ordered {
			if !c.At.After(cm.At) || c.SHA == cm.CommitSHA {
				continue
			}
			diff, err := m.Git(ctx, "-c", "core.quotePath=false", "diff", "-U0", "--no-color", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/", cm.CommitSHA, c.SHA, "--", cm.Path)
			if err != nil {
				break // the comment's commit is not in the mirror
			}
			if touches(oldRanges(string(diff), true)[cm.Path], cm.Line-3, cm.Line+3) {
				out = append(out, reviewChange{CommentID: cm.ID, SHA: c.SHA})
				break
			}
		}
	}
	return out
}

// oldRanges reads a -U0 diff and returns, per old path, the old-side line ranges its hunks
// change. With insertions, a hunk that only adds lines after line n counts as [n, n+1];
// otherwise it is left out.
func oldRanges(diff string, insertions bool) map[string][][2]int {
	out := map[string][][2]int{}
	path := ""
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			path = diffPath(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "@@ ") && path != "":
			start, count, ok := hunkOld(line)
			switch {
			case !ok:
			case count > 0:
				out[path] = append(out[path], [2]int{start, start + count - 1})
			case insertions:
				out[path] = append(out[path], [2]int{start, start + 1})
			}
		}
	}
	return out
}

func diffPath(name string) string {
	name = strings.TrimSuffix(name, "\t")
	if strings.HasPrefix(name, `"`) {
		if unquoted, err := strconv.Unquote(name); err == nil {
			name = unquoted
		}
	}
	if name == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(name, "a/")
}

// hunkOld parses the old side of "@@ -start[,count] +start[,count] @@".
func hunkOld(header string) (start, count int, ok bool) {
	fields := strings.Fields(header)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") {
		return 0, 0, false
	}
	s, c, hasCount := strings.Cut(strings.TrimPrefix(fields[1], "-"), ",")
	start, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, false
	}
	count = 1
	if hasCount {
		if count, err = strconv.Atoi(c); err != nil {
			return 0, 0, false
		}
	}
	return start, count, true
}

func touches(ranges [][2]int, from, to int) bool {
	for _, r := range ranges {
		if r[0] <= to && r[1] >= from {
			return true
		}
	}
	return false
}

func slicesSortedByTime(commits []prCommit) []prCommit {
	ordered := append([]prCommit(nil), commits...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })
	return ordered
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
