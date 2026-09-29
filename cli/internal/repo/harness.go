package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
)

// DefaultHarnessGlobs are the harness files of SDD section 13, for a casebox.yml that names none.
var DefaultHarnessGlobs = []string{"AGENTS.md", "CLAUDE.md", ".cursor/rules/**", ".claude/skills/**", ".agents/skills/**", ".mcp.json", ".cursor/mcp.json"}

// HarnessGlobs returns the globs of casebox.yml, or the default list.
func (c Config) HarnessGlobs() []string {
	if len(c.Harness.Globs) > 0 {
		return c.Harness.Globs
	}
	return DefaultHarnessGlobs
}

// Harness reads the harness files at commit from the repository in dir, a checkout or a bare
// mirror. A pattern matches the whole repo-relative path: "AGENTS.md" is the file at the root only.
func Harness(ctx context.Context, dir, commit string, globs []string) (*capture.Harness, error) {
	out, err := git(ctx, dir, "-c", "core.quotePath=false", "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, fmt.Errorf("list the files of %s: %w", commit, err)
	}
	var lines []string
	files := []string{}
	for _, entry := range strings.Split(out, "\x00") {
		meta, file, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" || !matchAny(globs, file) {
			continue
		}
		lines = append(lines, file+" "+fields[2])
		files = append(files, file)
	}
	sort.Strings(lines)
	sort.Strings(files)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return &capture.Harness{Hash: hex.EncodeToString(sum[:]), Files: files}, nil
}

// CommitBefore returns the last commit on branch at or before t, or on HEAD when the branch is
// unknown or gone.
func CommitBefore(ctx context.Context, dir, branch string, t time.Time) (string, error) {
	before := "--before=" + t.UTC().Format(time.RFC3339)
	if branch != "" && branch != "HEAD" {
		if sha, err := git(ctx, dir, "rev-list", "-1", before, "refs/heads/"+branch, "--"); err == nil && sha != "" {
			return sha, nil
		}
	}
	sha, err := git(ctx, dir, "rev-list", "-1", before, "HEAD", "--")
	if err != nil || sha == "" {
		return "", fmt.Errorf("no commit before %s", t.UTC().Format(time.RFC3339))
	}
	return sha, nil
}

// CommitsBetween counts the commits by the author email on any ref from start to end.
func CommitsBetween(ctx context.Context, dir, email string, start, end time.Time) (int, error) {
	out, err := git(ctx, dir, "log", "--all", "--fixed-strings", "--author="+email,
		"--since="+start.UTC().Format(time.RFC3339), "--until="+end.UTC().Format(time.RFC3339), "--format=%H")
	if err != nil {
		return 0, err
	}
	return len(strings.Fields(out)), nil
}

func matchAny(globs []string, file string) bool {
	for _, g := range globs {
		if MatchGlob(g, file) {
			return true
		}
	}
	return false
}

// MatchGlob matches a slash-separated path against a pattern in which "*" and "?" stay within one
// segment and a "**" segment matches any number of segments, including none. A pattern that ends
// in "/**" matches everything below that directory.
func MatchGlob(pattern, file string) bool {
	return matchSegments(strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(file, "/"))
}

func matchSegments(pattern, parts []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			if len(rest) == 0 {
				return len(parts) > 0
			}
			for i := 0; i <= len(parts); i++ {
				if matchSegments(rest, parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], parts[0]); err != nil || !ok {
			return false
		}
		pattern, parts = pattern[1:], parts[1:]
	}
	return len(parts) == 0
}

// MatchesAny reports whether a repo-relative path matches any of the globs, as Harness matches them.
func MatchesAny(globs []string, file string) bool { return matchAny(globs, file) }
