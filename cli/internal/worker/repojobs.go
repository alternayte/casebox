package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/gitmirror"
	"github.com/alternayte/casebox/cli/internal/pipeline"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sources/entire"
	"github.com/alternayte/casebox/cli/internal/sources/gitai"
	"go.yaml.in/yaml/v3"
)

// RepoJobs reads the extra refs of a repository: Entire checkpoints and git-ai notes.
type RepoJobs struct {
	Client     *api.Client
	MirrorRoot string
	Remotes    *gitmirror.Remotes // with the worker's tokens; the server never holds them
}

type repoPayload struct {
	Repo      string `json:"repo"`
	SinceDays int    `json:"sinceDays"`
}

const batchEvents = 1000

// Entire imports the sessions of checkpoints that commits name, through the same redaction and
// identity marking as local logs. Each session's person is the author of its commit.
func (r RepoJobs) Entire(ctx context.Context, job Job) (any, error) {
	p, m, err := r.open(ctx, job)
	if err != nil {
		return nil, err
	}
	since := time.Now().AddDate(0, 0, -p.SinceDays)
	sessions, err := entire.Read(ctx, m, since)
	if err != nil {
		return nil, err
	}
	cfg := mirrorConfig(ctx, m)
	names := authorNames(ctx, m)
	events := 0
	for _, s := range sessions {
		if s.AuthorEmail == "" {
			continue
		}
		pipe, err := pipeline.New("", cfg, repo.State{Repo: p.Repo}, capture.Mark("email", s.AuthorEmail), names)
		if err != nil {
			return nil, Permanent{err}
		}
		session := pipe.Session(ctx, s.Session)
		// A checkpoint exists because the session was committed, so it has at least one commit.
		one := 1
		session.Commits = &one
		session.Harness = mirrorHarness(ctx, m, cfg, session, s.CommitSHA)
		processed := pipe.Events(s.Events, pipeline.ModeFull)
		for start := 0; start < len(processed); start += batchEvents {
			end := min(start+batchEvents, len(processed))
			if err := r.Client.Do(ctx, http.MethodPost, "/worker/v1/sessions", map[string]any{"session": session, "events": processed[start:end]}, nil); err != nil {
				return nil, err
			}
		}
		events += len(processed)
	}
	return map[string]int{"sessions": len(sessions), "events": events}, nil
}

// GitAI sends the agent-written line ranges of every commit since the window that has a note.
// The human authors a note names are never sent.
func (r RepoJobs) GitAI(ctx context.Context, job Job) (any, error) {
	p, m, err := r.open(ctx, job)
	if err != nil {
		return nil, err
	}
	notes, err := m.Git(ctx, "notes", "--ref=ai", "list")
	if err != nil {
		return map[string]int{"commits": 0}, nil // no notes in this repository
	}
	since := time.Now().AddDate(0, 0, -p.SinceDays).Format(time.RFC3339)
	recent, err := m.Git(ctx, "log", "--branches", "--since="+since, "--format=%H")
	if err != nil {
		return nil, err
	}
	inWindow := map[string]bool{}
	for _, sha := range strings.Fields(string(recent)) {
		inWindow[sha] = true
	}

	type commit struct {
		Sha   string              `json:"sha"`
		Files []gitai.Attribution `json:"files"`
	}
	var batch []commit
	sent, files := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := r.Client.Do(ctx, http.MethodPost, "/worker/v1/attributions", map[string]any{"repo": p.Repo, "commits": batch}, nil)
		batch = batch[:0]
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(notes)), "\n") {
		blob, sha, ok := strings.Cut(line, " ")
		if !ok || !inWindow[sha] {
			continue
		}
		body, err := m.Show(ctx, blob)
		if err != nil {
			continue
		}
		attributions, err := gitai.Parse(string(body))
		if err != nil || len(attributions) == 0 {
			continue
		}
		batch = append(batch, commit{Sha: sha, Files: attributions})
		sent++
		files += len(attributions)
		if len(batch) == 100 {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return map[string]int{"commits": sent, "files": files}, nil
}

func (r RepoJobs) open(ctx context.Context, job Job) (repoPayload, *gitmirror.Mirror, error) {
	var p repoPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.Repo == "" {
		return p, nil, Permanent{fmt.Errorf("the job names no repository")}
	}
	if p.SinceDays <= 0 {
		p.SinceDays = 183
	}
	m, err := gitmirror.Open(ctx, r.MirrorRoot, p.Repo, r.Remotes)
	return p, m, err
}

// The team's capture rules come from casebox.yml on the default branch, when there is one.
func mirrorConfig(ctx context.Context, m *gitmirror.Mirror) repo.Config {
	var cfg repo.Config
	if data, err := m.Show(ctx, "HEAD:"+repo.ConfigPath); err == nil {
		_ = yaml.Unmarshal(data, &cfg)
	}
	return cfg
}

// mirrorHarness reads the harness at the last commit on the session's branch before it started,
// or at the parent of the commit that kept the checkpoint when the branch is gone.
func mirrorHarness(ctx context.Context, m *gitmirror.Mirror, cfg repo.Config, s capture.Session, commit string) *capture.Harness {
	at := s.HeadStart
	if at == "" && s.Branch != "" {
		out, err := m.Git(ctx, "rev-list", "-1", "--before="+s.StartedAt.UTC().Format(time.RFC3339), "refs/heads/"+s.Branch, "--")
		if err == nil {
			at = strings.TrimSpace(string(out))
		}
	}
	if at == "" {
		at = commit + "^"
	}
	h, err := repo.Harness(ctx, m.Dir, at, cfg.HarnessGlobs())
	if err != nil {
		return nil
	}
	return h
}

func authorNames(ctx context.Context, m *gitmirror.Mirror) []string {
	out, err := m.Git(ctx, "log", "--branches", "-5000", "--format=%an%n%cn")
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}
