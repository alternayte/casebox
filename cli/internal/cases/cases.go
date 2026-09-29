// Package cases runs the worker side of Casebox's cases (docs/specs/cases.md, "Jobs"): mining a
// repository's merged pull requests and corrections into case candidates, validating a case in a
// sandbox with the 3-of-3 flake filter, and drafting its instruction with the analysis model.
package cases

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/gitmirror"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox"
)

// Case kinds, scopes and repository roles, as the server writes them.
const (
	Capability = "capability"
	Regression = "regression"
	Steering   = "steering"

	ScopeSingle = "single"
	ScopeSplit  = "split"
	ScopeMulti  = "multi"

	RoleSealed  = "sealed"
	RoleContext = "context"
)

// Jobs runs the three case jobs on this host. Mine needs only git; Validate needs Provider;
// Instruction needs Model.
type Jobs struct {
	Client      *api.Client
	MirrorRoot  string
	GitHubToken string
	Provider    sandbox.Provider
	Model       *analysis.Client
	Now         func() time.Time
}

func (j Jobs) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

// source opens the repository of a workspace, fetched, and returns its git directory.
type source func(ctx context.Context, repo string) (string, error)

func (j Jobs) mirrors() source {
	opened := map[string]string{}
	return func(ctx context.Context, name string) (string, error) {
		if dir, ok := opened[name]; ok {
			return dir, nil
		}
		m, err := gitmirror.Open(ctx, j.MirrorRoot, name, j.GitHubToken)
		if err != nil {
			return "", err
		}
		opened[name] = m.Dir
		return m.Dir, nil
	}
}

// Pull is a merged pull request of a candidate.
type Pull struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	BaseRef  string    `json:"baseRef"`
	BaseSha  string    `json:"baseSha"`
	MergeSha string    `json:"mergeSha"`
	MergedAt time.Time `json:"mergedAt"`
	Bot      bool      `json:"bot"`
}

// Correction is the steering correction a steering candidate comes from.
type Correction struct {
	Ref       string  `json:"ref"`
	Signal    string  `json:"signal"`
	WentWrong string  `json:"wentWrong"`
	Text      *string `json:"text"`
}

// CaseRepo is one repository of a case: the agent works on a sealed one; a context one is present
// at its merged state.
type CaseRepo struct {
	Repo   string  `json:"repo"`
	Base   string  `json:"base"`
	Merged *string `json:"merged"`
	Role   string  `json:"role"`
}

// git runs git in dir and returns its standard output; the error carries git's message.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitIn(ctx, dir, nil, args...)
}

func gitIn(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	full := append([]string{"-c", "core.quotePath=false", "-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// defaultBranch is the branch HEAD names: in a mirror, the remote's default branch.
func defaultBranch(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("find the default branch: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// hasCommit reports whether sha names a commit the repository has.
func hasCommit(ctx context.Context, dir, sha string) bool {
	if sha == "" || strings.HasPrefix(sha, "-") {
		return false
	}
	_, err := git(ctx, dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// firstParent is the commit a merge, squash or rebase commit landed on: the base of its change.
func firstParent(ctx context.Context, dir, sha string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--verify", sha+"^1^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s has no parent", short(sha))
	}
	return strings.TrimSpace(string(out)), nil
}

// config reads casebox.yml at the default branch; a repository without one gets the defaults.
func config(ctx context.Context, dir string) repo.Config {
	var cfg repo.Config
	if data, err := repo.ReadAt(ctx, dir, "HEAD", repo.ConfigPath); err == nil {
		_ = yaml.Unmarshal(data, &cfg)
	}
	return cfg
}

// changedFiles lists the paths the change from base to merged touches, renames as a delete and an
// add.
func changedFiles(ctx context.Context, dir, base, merged string) ([]string, error) {
	out, err := git(ctx, dir, "diff", "--name-only", "--no-renames", "-z", base, merged, "--")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// diff is the binary git diff from base to merged, with prefix before every path.
func diff(ctx context.Context, dir, base, merged, prefix string) ([]byte, error) {
	return git(ctx, dir, "diff", "--binary", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--src-prefix=a/"+prefix, "--dst-prefix=b/"+prefix, base, merged, "--")
}

// FilePatch is one file's part of a git diff.
type FilePatch struct {
	Path string
	Body []byte
}

// SplitPatch cuts a git diff made with --no-renames into one part per file, in order.
func SplitPatch(d []byte) ([]FilePatch, error) {
	var out []FilePatch
	start := -1
	for i := 0; i < len(d); {
		end := bytes.IndexByte(d[i:], '\n')
		if end < 0 {
			end = len(d)
		} else {
			end += i + 1
		}
		if bytes.HasPrefix(d[i:end], []byte("diff --git ")) {
			if start >= 0 {
				out[len(out)-1].Body = d[start:i]
			}
			p, err := headerPath(strings.TrimRight(string(d[i+len("diff --git "):end]), "\r\n"))
			if err != nil {
				return nil, err
			}
			out = append(out, FilePatch{Path: p})
			start = i
		} else if start < 0 && len(bytes.TrimSpace(d[i:end])) > 0 {
			return nil, errors.New("the diff does not start with a diff --git header")
		}
		i = end
	}
	if start >= 0 {
		out[len(out)-1].Body = d[start:]
	}
	return out, nil
}

// headerPath reads the path of a "diff --git a/<p> b/<p>" header without renames: both halves
// name the same path, quoted when it has special characters.
func headerPath(h string) (string, error) {
	if strings.HasPrefix(h, `"`) {
		end := closingQuote(h)
		if end < 0 {
			return "", fmt.Errorf("unreadable diff header %q", h)
		}
		p, err := strconv.Unquote(h[:end+1])
		if err != nil {
			return "", fmt.Errorf("unreadable diff header %q", h)
		}
		return strings.TrimPrefix(p, "a/"), nil
	}
	n := (len(h) - 1) / 2
	if len(h)%2 == 0 || h[n] != ' ' || !strings.HasPrefix(h, "a/") || h[n+1:n+3] != "b/" || h[2:n] != h[n+3:] {
		return "", fmt.Errorf("unreadable diff header %q", h)
	}
	return h[2:n], nil
}

func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// Change is a change split for a case: the source patch the agent must reproduce and the test
// patch that holds the held-out tests.
type Change struct {
	Source      []byte
	Tests       []byte
	TestFiles   []string // with the layout prefix
	SourceFiles []string
	Dirs        []string // repo-relative directories of every changed file
}

// splitChange classifies each file of a diff made with prefix as test or source by globs.
func splitChange(d []byte, prefix string, globs []string) (Change, error) {
	parts, err := SplitPatch(d)
	if err != nil {
		return Change{}, err
	}
	var c Change
	dirs := map[string]bool{}
	for _, p := range parts {
		rel := strings.TrimPrefix(p.Path, prefix)
		dirs[path.Dir(rel)] = true
		if repo.IsTestFile(globs, rel) {
			c.Tests = append(c.Tests, p.Body...)
			c.TestFiles = append(c.TestFiles, p.Path)
		} else {
			c.Source = append(c.Source, p.Body...)
			c.SourceFiles = append(c.SourceFiles, p.Path)
		}
	}
	c.Dirs = sortedKeys(dirs)
	return c, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func ptr[T any](v T) *T { return &v }
