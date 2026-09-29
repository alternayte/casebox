package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alternayte/casebox/cli/internal/gitmirror"
	"github.com/alternayte/casebox/cli/internal/recipe"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox"
)

// Environments prepares confirmed recipes in this host's sandbox provider, so case runs start
// from its cache (docs/specs/sandboxes.md, env.build).
type Environments struct {
	Provider    sandbox.Provider
	Name        string
	MirrorRoot  string
	GitHubToken string
}

type envBuildPayload struct {
	Workspace string      `json:"workspace"`
	Repo      string      `json:"repo"`
	Recipe    repo.Recipe `json:"recipe"`
	Hash      string      `json:"hash"`
}

// Build reads the recipe's lockfiles from the repository's default branch and prepares the
// environment. A failed build is retried, since a registry or network can fail for a while.
func (e Environments) Build(ctx context.Context, job Job) (any, error) {
	var p envBuildPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.Repo == "" {
		return nil, Permanent{errors.New("the job names no repository or recipe")}
	}
	if err := p.Recipe.Validate(); err != nil {
		return nil, Permanent{err}
	}
	m, err := gitmirror.Open(ctx, e.MirrorRoot, p.Repo, e.GitHubToken)
	if err != nil {
		return nil, err
	}
	commit, err := repo.Resolve(ctx, m.Dir, "HEAD")
	if err != nil {
		return nil, err
	}
	files, err := repo.TrackedFiles(ctx, m.Dir, commit)
	if err != nil {
		return nil, err
	}
	spec, err := recipe.Spec(p.Recipe, files, func(f string) ([]byte, error) { return repo.ReadAt(ctx, m.Dir, commit, f) })
	if err != nil {
		return nil, err
	}
	start := time.Now()
	image, err := e.Provider.Prepare(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("prepare the environment of %s at %s: %w", p.Repo, commit[:12], err)
	}
	return map[string]any{"key": image.Key, "image": image.ID, "provider": e.Name, "commit": commit, "seconds": time.Since(start).Seconds()}, nil
}
