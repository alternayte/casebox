// Package importer reads the session logs already on this machine (Claude Code transcripts,
// Codex rollouts) for one enrolled repository and puts them in the spool.
package importer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/pipeline"
	"github.com/alternayte/casebox/cli/internal/sources/claudecode"
	"github.com/alternayte/casebox/cli/internal/sources/codex"
	"github.com/alternayte/casebox/cli/internal/spool"
)

// Summary counts what an import found.
type Summary struct {
	Sessions    map[string]int // per agent
	Events      int
	Unsupported int // Codex rollouts in the pre-2025 format
	Failed      int
}

// Options select what to import.
type Options struct {
	Home  string
	Since time.Time
	Mode  string
}

// Import reads every session since opts.Since that ran inside the repository r.
func Import(ctx context.Context, r *pipeline.Repo, s *spool.Spool, opts Options) (Summary, error) {
	sum := Summary{Sessions: map[string]int{}}
	add := func(session capture.Session, cwd string, events []capture.Event) error {
		if !inside(r.Root, cwd) || session.StartedAt.Before(opts.Since) {
			return nil
		}
		if err := s.Add(ctx, r.Session(session), r.Events(events, opts.Mode)); err != nil {
			return err
		}
		sum.Sessions[session.Agent]++
		sum.Events += len(events)
		return nil
	}

	claude, _ := filepath.Glob(filepath.Join(opts.Home, ".claude", "projects", "*", "*.jsonl"))
	for _, path := range recent(claude, opts.Since) {
		res, err := ParseFile(path, claudecode.Agent)
		if err != nil {
			sum.Failed++
			continue
		}
		if err := add(res.Session, res.Cwd, res.Events); err != nil {
			return sum, err
		}
	}

	var rollouts []string
	_ = filepath.WalkDir(filepath.Join(opts.Home, ".codex", "sessions"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			rollouts = append(rollouts, p)
		}
		return nil
	})
	for _, path := range recent(rollouts, opts.Since) {
		res, err := ParseFile(path, codex.Agent)
		switch {
		case errors.Is(err, codex.ErrUnsupportedFormat):
			sum.Unsupported++
			continue
		case err != nil:
			sum.Failed++
			continue
		case res.Subagent:
			continue
		}
		if err := add(res.Session, res.Cwd, res.Events); err != nil {
			return sum, err
		}
	}
	return sum, ctx.Err()
}

// Parsed is a parsed log of either agent.
type Parsed struct {
	Session  capture.Session
	Cwd      string
	Subagent bool
	Events   []capture.Event
}

// ParseFile parses one transcript or rollout.
func ParseFile(path, agent string) (Parsed, error) {
	f, err := os.Open(path)
	if err != nil {
		return Parsed{}, err
	}
	defer f.Close()
	switch agent {
	case claudecode.Agent:
		res, err := claudecode.Parse(f)
		return Parsed{Session: res.Session, Cwd: res.Cwd, Events: res.Events}, err
	case codex.Agent:
		res, err := codex.Parse(f)
		return Parsed{Session: res.Session, Cwd: res.Cwd, Subagent: res.Subagent, Events: res.Events}, err
	default:
		return Parsed{}, errors.New("no transcript parser for " + agent)
	}
}

func recent(paths []string, since time.Time) []string {
	var out []string
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && !info.ModTime().Before(since) {
			out = append(out, p)
		}
	}
	return out
}

func inside(root, dir string) bool {
	if dir == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dir = d
	}
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
