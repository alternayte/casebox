// Package pipeline prepares a parsed session for the spool: it ties the session to its enrolled
// repository, marks the developer and every identity, redacts secrets and applies the prompt mode.
// Every source (import, hooks, the background transcript import) goes through it.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/repo"
)

// Prompt modes, as the server reports them.
const (
	ModeOff      = "off"
	ModeRedacted = "redacted"
	ModeFull     = "full"
)

// Repo is what the pipeline knows about one enrolled repository.
type Repo struct {
	Root     string
	Config   repo.Config
	State    repo.State
	Person   string // the identity mark of the developer
	email    string // the developer's git user.email, to count their commits; never sent
	redactor *capture.Redactor
	marker   *capture.Marker
}

// ErrNotEnrolled means the session ran outside an enrolled repository; it is not captured.
var ErrNotEnrolled = repo.ErrNotEnrolled

// Open reads the enrolled repository that contains dir.
func Open(ctx context.Context, dir string) (*Repo, error) {
	if dir == "" {
		return nil, ErrNotEnrolled
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, ErrNotEnrolled
	}
	root, cfg, err := repo.LoadConfig(ctx, dir)
	if err != nil {
		return nil, err
	}
	email, err := repo.UserEmail(ctx, root)
	if err != nil {
		return nil, err
	}
	r, err := New(root, cfg, repo.Current(ctx, root), capture.Mark("email", email), authorNames(ctx, root))
	if err != nil {
		return nil, err
	}
	r.email = email
	return r, nil
}

// New builds a pipeline from parts, for the worker, which reads a mirror instead of a checkout.
func New(root string, cfg repo.Config, state repo.State, person string, names []string) (*Repo, error) {
	redactor, err := capture.NewRedactor(cfg.Capture.Redact)
	if err != nil {
		return nil, errors.New("a capture.redact pattern in casebox.yml is not a valid regular expression: " + err.Error())
	}
	return &Repo{Root: root, Config: cfg, State: state, Person: person, redactor: redactor, marker: capture.NewMarker(names)}, nil
}

// Session fills in what the repository knows about a session: its harness at the start and, once
// it has ended, how many commits the developer made.
func (r *Repo) Session(ctx context.Context, s capture.Session) capture.Session {
	s.Person = r.Person
	if s.Repo == "" {
		s.Repo = r.State.Repo
	}
	if s.Branch == "" {
		s.Branch = r.State.Branch
	}
	if s.WorkItem == "" {
		s.WorkItem = config.CurrentLink(r.Root)
	}
	if s.Harness == nil {
		s.Harness = r.harness(ctx, s)
	}
	if s.Commits == nil {
		s.Commits = r.Commits(ctx, s)
	}
	return s
}

// Commits counts the developer's commits in the repository from the session's start to 30
// minutes after its end, or returns nil while the end is unknown.
func (r *Repo) Commits(ctx context.Context, s capture.Session) *int {
	if s.EndedAt == nil || r.email == "" || r.Root == "" {
		return nil
	}
	n, err := repo.CommitsBetween(ctx, r.Root, r.email, s.StartedAt, s.EndedAt.Add(30*time.Minute))
	if err != nil {
		return nil
	}
	return &n
}

// The harness is read at the session's first commit, HEAD at start when the hooks saw it, else
// the last commit on its branch before it started. It is cached per commit and glob list, since
// many sessions start from the same commit and listing a large tree is slow.
func (r *Repo) harness(ctx context.Context, s capture.Session) *capture.Harness {
	if r.Root == "" {
		return nil
	}
	commit := s.HeadStart
	if commit == "" {
		var err error
		if commit, err = repo.CommitBefore(ctx, r.Root, s.Branch, s.StartedAt); err != nil {
			return nil
		}
	}
	globs := r.Config.HarnessGlobs()
	path, err := config.Path("cache", "harness", hash(strings.Join(globs, "\n"))+"-"+commit+".json")
	if err == nil {
		var h capture.Harness
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &h) == nil {
			return &h
		}
	}
	h, err := repo.Harness(ctx, r.Root, commit, globs)
	if err != nil {
		return nil
	}
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		if data, err := json.Marshal(h); err == nil {
			_ = os.WriteFile(path, data, 0o600)
		}
	}
	return h
}

// Events redacts and marks every event, makes file paths relative to the repository, and keeps
// only the text the prompt mode allows.
func (r *Repo) Events(events []capture.Event, mode string) []capture.Event {
	out := make([]capture.Event, 0, len(events))
	for _, e := range events {
		if keepText(mode, e.Kind) {
			e.Text = r.marker.MarkText(r.redactor.Redact(e.Text))
		} else {
			e.Text = ""
		}
		if len(e.Attrs) > 0 {
			attrs := make(map[string]string, len(e.Attrs))
			for k, v := range e.Attrs {
				attrs[k] = r.marker.MarkText(r.redactor.Redact(v))
			}
			e.Attrs = attrs
		}
		if e.Tool != nil && len(e.Tool.Files) > 0 {
			tool := *e.Tool
			tool.Files = make([]string, 0, len(e.Tool.Files))
			for _, f := range e.Tool.Files {
				tool.Files = append(tool.Files, r.redactor.Redact(r.relative(f)))
			}
			e.Tool = &tool
		}
		out = append(out, e)
	}
	return out
}

func keepText(mode, kind string) bool {
	switch mode {
	case ModeFull:
		return true
	case ModeRedacted:
		return kind != capture.KindToolResult && kind != capture.KindToolCall
	default:
		return false
	}
}

func (r *Repo) relative(path string) string {
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(path)
	}
	if r.Root == "" {
		return filepath.Base(path)
	}
	// git reports the root with symlinks resolved (/private/var on macOS); agents report paths as
	// they see them (/var), so both sides are resolved before comparing.
	for _, candidate := range []string{path, resolve(path)} {
		for _, root := range []string{r.Root, resolve(r.Root)} {
			if rel, err := filepath.Rel(root, candidate); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.Base(path)
}

// resolve follows symlinks in the longest existing prefix of path; the file itself may be gone.
func resolve(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	dir, base := filepath.Split(filepath.Clean(path))
	if dir == "" || filepath.Clean(dir) == path {
		return path
	}
	return filepath.Join(resolve(filepath.Clean(dir)), base)
}

// Author names change rarely and git log is slow on large repositories, so the names are cached
// for a day per repository.
func authorNames(ctx context.Context, root string) []string {
	path, err := config.Path("cache", "authors-"+hash(root)+".json")
	if err == nil {
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
			var names []string
			if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &names) == nil {
				return names
			}
		}
	}
	names := repo.AuthorNames(ctx, root, 5000)
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		if data, err := json.Marshal(names); err == nil {
			_ = os.WriteFile(path, data, 0o600)
		}
	}
	return names
}
