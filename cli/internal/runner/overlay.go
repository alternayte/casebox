package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox"
)

// Overlay puts a harness over a sealed sandbox's base tree: it removes every base file the globs
// match, then writes harnessFiles (repo-relative path to contents, each matched by the globs).
// With none it only removes. The base commit is rewritten to hold the result, so the agent's diff
// never carries the overlay and the repository still has one commit and no trace of the old one.
// The returned Sandbox carries the new base commit.
func Overlay(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, harnessFiles map[string][]byte, globs []string, none bool) (sandbox.Sandbox, error) {
	if sb.Meta[MetaBase] == "" {
		return sandbox.Sandbox{}, errors.New("the sandbox was not sealed: it has no base commit")
	}
	if none && len(harnessFiles) > 0 {
		return sandbox.Sandbox{}, errors.New("the harness none has no files")
	}
	written := make([]string, 0, len(harnessFiles))
	for name := range harnessFiles {
		clean, err := cleanPath(name)
		if err != nil || clean != name {
			return sandbox.Sandbox{}, fmt.Errorf("harness file %q is not a clean path inside the repository", name)
		}
		if dropped(clean, nil) || !matchAny(globs, clean) {
			return sandbox.Sandbox{}, fmt.Errorf("harness file %s is not matched by the harness globs", name)
		}
		written = append(written, clean)
	}
	sort.Strings(written)

	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", gitSafe + " ls-files -z"}, Env: gitEnv})
	if err != nil {
		return sandbox.Sandbox{}, err
	}
	if res.ExitCode != 0 || res.Truncated {
		return sandbox.Sandbox{}, fmt.Errorf("list the base files: %s", tail(string(res.Stderr), 20))
	}
	var removed []string
	for _, f := range strings.Split(string(res.Stdout), "\x00") {
		if f != "" && matchAny(globs, f) {
			removed = append(removed, f)
		}
	}

	if len(removed) > 0 {
		var list bytes.Buffer
		for _, f := range removed {
			list.WriteString(f)
			list.WriteByte(0)
		}
		if err := run(ctx, p, sb, sandbox.Command{Args: []string{"xargs", "-0", "rm", "-f", "--"}, Stdin: &list}, "remove the base harness"); err != nil {
			return sandbox.Sandbox{}, err
		}
		// Directories the removal emptied go too, deepest first, so none leaves no trace of them.
		dirs := parentDirs(removed)
		if len(dirs) > 0 {
			script := `for d in "$@"; do rmdir "$d" 2>/dev/null || true; done`
			if err := run(ctx, p, sb, sandbox.Command{Args: append([]string{"sh", "-c", script, "sh"}, dirs...)}, "remove emptied directories"); err != nil {
				return sandbox.Sandbox{}, err
			}
		}
	}
	if len(written) > 0 {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, name := range written {
			body := harnessFiles[name]
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatPAX}); err != nil {
				return sandbox.Sandbox{}, err
			}
			if _, err := tw.Write(body); err != nil {
				return sandbox.Sandbox{}, err
			}
		}
		if err := tw.Close(); err != nil {
			return sandbox.Sandbox{}, err
		}
		if err := run(ctx, p, sb, sandbox.Command{Args: []string{"tar", "-x", "-f", "-", "-C", sb.Workdir}, Stdin: &buf}, "write the harness"); err != nil {
			return sandbox.Sandbox{}, err
		}
	}

	var list bytes.Buffer
	for _, f := range append(removed, written...) {
		list.WriteString(f)
		list.WriteByte(0)
	}
	script := `set -e
` + gitSafe + ` update-index --add --remove -z --stdin
commit=$(git commit-tree -m base "$(git write-tree)")
git -c core.logAllRefUpdates=false update-ref ` + baseRef + ` "$commit"
git reflog expire --expire=now --all
git prune --expire=now
echo "$commit"`
	res, err = p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", script}, Stdin: &list, Env: commitEnv()})
	if err != nil {
		return sandbox.Sandbox{}, err
	}
	if res.ExitCode != 0 {
		return sandbox.Sandbox{}, fmt.Errorf("commit the harness overlay: %s", tail(string(res.Stderr), 20))
	}
	return withBase(sb, strings.TrimSpace(string(res.Stdout))), nil
}

func matchAny(globs []string, file string) bool {
	for _, g := range globs {
		if repo.MatchGlob(g, file) {
			return true
		}
	}
	return false
}

// parentDirs are the directories above files, deepest first.
func parentDirs(files []string) []string {
	seen := map[string]bool{}
	for _, f := range files {
		for d := path.Dir(f); d != "." && d != "/" && !seen[d]; d = path.Dir(d) {
			seen[d] = true
		}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/"); di != dj {
			return di > dj
		}
		return dirs[i] < dirs[j]
	})
	return dirs
}

// UserFiles writes a shared harness's files into the sandbox user's home, where the agent reads its
// user-level configuration (docs/specs/harness-ci.md). Paths are relative to home.
func UserFiles(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, home string, files map[string][]byte) error {
	if len(files) == 0 {
		return nil
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatPAX}); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return run(ctx, p, sb, sandbox.Command{Args: []string{"tar", "-x", "-f", "-", "-C", home}, Stdin: &buf}, "write the shared harness")
}
