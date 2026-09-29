// Package runner seals a case into sandboxes and verifies what an agent did (docs/specs/cases.md,
// "Sealing", "Validation", "Harness overlay and drift"). The agent works in a sandbox that holds
// one fresh commit of the base tree and nothing of its history; the verifier is another sandbox
// that rebuilds the pristine base, applies the agent's diff and the held-out tests, and runs the
// test commands. The runner returns raw result files; the oracle package parses them.
package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/egressproxy"
)

// MetaBase is the Sandbox.Meta key that holds the base commit Seal made.
const MetaBase = "runner.base"

// The base commit's author and committer, and its date: the same tree always gives the same commit.
const (
	baseName  = "casebox"
	baseEmail = "casebox@localhost"
	baseDate  = "946684800 +0000" // 2000-01-01T00:00:00Z
	baseRef   = "refs/heads/main"
)

// gitHosts are hosts no sandbox may reach: they serve the repository's history, including the
// fix a case asks for.
var gitHosts = []string{
	"github.com", "api.github.com", "codeload.github.com", "raw.githubusercontent.com", "objects.githubusercontent.com",
	"gitlab.com", "bitbucket.org", "api.bitbucket.org",
}

// gitEnv makes every git command the runner runs ignore the image's system and global config.
var gitEnv = map[string]string{
	"GIT_CONFIG_NOSYSTEM": "1",
	"GIT_CONFIG_GLOBAL":   "/dev/null",
	"GIT_TERMINAL_PROMPT": "0",
}

// gitSafe are the -c options every git command the runner runs takes, so hooks, fsmonitor and
// diff settings in the repository's config cannot change what it does.
const gitSafe = "git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c core.quotePath=true"

// Seal starts the agent's sandbox from spec with network none, the egress allow-list, the
// registry mirror when mirror is set (DeniedPackages gives its denied list) and env (the model key
// goes here and nowhere else), and puts the base tree (a tar stream) in its working
// directory as a fresh repository with one commit, without the held-out test files, .casebox/ and
// any .git. The returned Sandbox carries the base commit in Meta[MetaBase]. The image must have
// git; the recipe provides it.
func Seal(ctx context.Context, p sandbox.Provider, spec sandbox.EnvSpec, tree io.Reader, heldOut []string, egress []string, mirror *sandbox.Mirror, env map[string]string) (sandbox.Sandbox, error) {
	if err := CheckEgress(egress); err != nil {
		return sandbox.Sandbox{}, err
	}
	image, err := p.Prepare(ctx, spec)
	if err != nil {
		return sandbox.Sandbox{}, fmt.Errorf("prepare the environment: %w", err)
	}
	sb, err := p.Start(ctx, image, sandbox.StartOptions{Network: sandbox.NetworkNone, Egress: egress, Mirror: mirror, Env: env})
	if err != nil {
		return sandbox.Sandbox{}, fmt.Errorf("start the agent's sandbox: %w", err)
	}
	base, err := commitBase(ctx, p, sb, tree, heldOut)
	if err != nil {
		_ = p.Destroy(context.WithoutCancel(ctx), sb)
		return sandbox.Sandbox{}, err
	}
	return withBase(sb, base), nil
}

// AgentDiff is every change the agent made against the base commit, new files included, as a
// binary git diff. Files the repository's ignore rules match (build output, dependency folders)
// are not in it.
func AgentDiff(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox) ([]byte, error) {
	base := sb.Meta[MetaBase]
	if base == "" {
		return nil, errors.New("the sandbox was not sealed: it has no base commit")
	}
	script := `set -e
d=$(mktemp -d)
` + gitSafe + ` add -A
` + gitSafe + ` -c diff.noprefix=false -c diff.mnemonicPrefix=false diff --cached --binary --no-color --no-ext-diff --no-textconv --no-renames --no-relative --src-prefix=a/ --dst-prefix=b/ "$1" -- >"$d/agent.diff"
echo "$d"`
	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", script, "sh", base}, Env: gitEnv})
	if err != nil {
		return nil, fmt.Errorf("take the agent's diff: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("take the agent's diff: %s", tail(string(res.Stderr), 20))
	}
	dir := strings.TrimSpace(string(res.Stdout))
	files, err := copyFiles(ctx, p, sb, dir+"/agent.diff", maxDiff)
	if err != nil {
		return nil, fmt.Errorf("copy the agent's diff out: %w", err)
	}
	_, _ = p.Exec(ctx, sb, sandbox.Command{Args: []string{"rm", "-rf", dir}})
	body, ok := files["agent.diff"]
	if !ok {
		return nil, errors.New("copy the agent's diff out: the diff file is missing")
	}
	return body, nil
}

// maxDiff caps an agent's diff.
const maxDiff = 256 << 20

// CheckEgress refuses an allow-list that is not valid or that reaches a git host.
func CheckEgress(egress []string) error {
	allow, err := egressproxy.ParseAllow(egress)
	if err != nil {
		return err
	}
	for _, h := range gitHosts {
		if allow.Permits(h) {
			return fmt.Errorf("the egress allow-list reaches the git host %s; no sandbox may", h)
		}
	}
	return nil
}

// commitBase puts tree in the sandbox's working directory, minus the dropped paths, and commits it
// as the only commit of a fresh repository. It returns the commit.
func commitBase(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, tree io.Reader, heldOut []string) (string, error) {
	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"git", "--version"}, Env: gitEnv})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", errors.New("the environment has no git: the recipe's image or install steps must provide it, because the runner commits the base tree in the sandbox")
	}
	drop := map[string]bool{}
	for _, h := range heldOut {
		clean, err := cleanPath(h)
		if err != nil {
			return "", fmt.Errorf("held-out file: %w", err)
		}
		drop[clean] = true
	}
	// A .git or .casebox in the image, or a held-out file among its context files, goes first.
	rm := append([]string{"rm", "-rf", "--", ".git", ".casebox"}, sortedKeys(drop)...)
	if err := run(ctx, p, sb, sandbox.Command{Args: rm}, "clear the working directory"); err != nil {
		return "", err
	}

	// The filtered tree is spooled to a file, so a failed filter never leaves half a tree behind.
	spool, err := os.CreateTemp("", "casebox-tree-*.tar")
	if err != nil {
		return "", err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	files, err := filterTree(tree, spool, drop)
	if err != nil {
		return "", err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if err := run(ctx, p, sb, sandbox.Command{Args: []string{"tar", "-x", "-f", "-", "-C", sb.Workdir}, Stdin: spool}, "copy the base tree in"); err != nil {
		return "", err
	}

	var list bytes.Buffer
	for _, f := range files {
		list.WriteString(f)
		list.WriteByte(0)
	}
	script := `set -e
git init -q
git config user.name casebox
git config user.email casebox@localhost
git config commit.gpgsign false
` + gitSafe + ` update-index --add -z --stdin
commit=$(git commit-tree -m base "$(git write-tree)")
git -c core.logAllRefUpdates=false symbolic-ref HEAD ` + baseRef + `
git -c core.logAllRefUpdates=false update-ref ` + baseRef + ` "$commit"
rm -rf .git/logs
echo "$commit"`
	env := commitEnv()
	res, err = p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", script}, Stdin: &list, Env: env})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("commit the base tree: %s", tail(string(res.Stderr), 20))
	}
	base := strings.TrimSpace(string(res.Stdout))
	if err := excludeUntracked(ctx, p, sb); err != nil {
		return "", err
	}
	return base, nil
}

// excludeUntracked ignores, in .git/info/exclude, what the environment's install steps left in the
// working directory (dependency folders, build caches): it is not the agent's work.
func excludeUntracked(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox) error {
	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", gitSafe + " ls-files -z --others --exclude-standard --directory"}, Env: gitEnv})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 || res.Truncated {
		return fmt.Errorf("list what the environment left in the working directory: %s", tail(string(res.Stderr), 20))
	}
	var lines strings.Builder
	for _, p := range strings.Split(string(res.Stdout), "\x00") {
		if p != "" {
			lines.WriteString("/" + escapeIgnore(p) + "\n")
		}
	}
	if lines.Len() == 0 {
		return nil
	}
	script := `mkdir -p .git/info && cat >>.git/info/exclude && test -z "$(` + gitSafe + ` status --porcelain --untracked-files=all)"`
	res, err = p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", script}, Stdin: strings.NewReader(lines.String()), Env: gitEnv})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("the base repository is not clean after ignoring what the environment left: %s", tail(string(res.Stderr), 20))
	}
	return nil
}

// filterTree copies the regular files, symlinks and directories of tree to w, without the dropped
// paths, .casebox/, any .git and the pax global header (which carries the commit id). It returns
// the file paths it copied, for the index.
func filterTree(tree io.Reader, w io.Writer, drop map[string]bool) ([]string, error) {
	r := tar.NewReader(tree)
	out := tar.NewWriter(w)
	var files []string
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the base tree: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeSymlink, tar.TypeDir:
		default:
			continue
		}
		name, err := cleanPath(strings.TrimSuffix(h.Name, "/"))
		if err != nil {
			return nil, fmt.Errorf("the base tree: %w", err)
		}
		if dropped(name, drop) {
			continue
		}
		hdr := &tar.Header{Typeflag: h.Typeflag, Name: name, Linkname: h.Linkname, Mode: h.Mode & 0o777, Size: h.Size, ModTime: h.ModTime, Format: tar.FormatPAX}
		if h.Typeflag == tar.TypeDir {
			hdr.Name += "/"
			hdr.Size = 0
		}
		if h.Typeflag == tar.TypeSymlink {
			hdr.Size = 0
		}
		if err := out.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.Copy(out, r); err != nil {
				return nil, fmt.Errorf("read the base tree: %w", err)
			}
		}
		if h.Typeflag != tar.TypeDir {
			files = append(files, name)
		}
	}
	return files, out.Close()
}

func dropped(name string, drop map[string]bool) bool {
	parts := strings.Split(name, "/")
	if parts[0] == ".casebox" {
		return true
	}
	for i, part := range parts {
		if part == ".git" {
			return true
		}
		if drop[strings.Join(parts[:i+1], "/")] {
			return true
		}
	}
	return false
}

// cleanPath is a slash-separated path inside the working directory, without "./".
func cleanPath(name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if name == "" || clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "\x00") {
		return "", fmt.Errorf("%q is not a path inside the repository", name)
	}
	return clean, nil
}

// escapeIgnore makes a path a literal gitignore pattern.
func escapeIgnore(p string) string {
	var b strings.Builder
	for i, c := range p {
		if strings.ContainsRune(`*?[\`, c) || (i == 0 && (c == '#' || c == '!')) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	if strings.HasSuffix(p, " ") {
		return strings.TrimSuffix(b.String(), " ") + `\ `
	}
	return b.String()
}

func commitEnv() map[string]string {
	env := map[string]string{
		"GIT_AUTHOR_NAME": baseName, "GIT_AUTHOR_EMAIL": baseEmail, "GIT_AUTHOR_DATE": baseDate,
		"GIT_COMMITTER_NAME": baseName, "GIT_COMMITTER_EMAIL": baseEmail, "GIT_COMMITTER_DATE": baseDate,
	}
	for k, v := range gitEnv {
		env[k] = v
	}
	return env
}

func withBase(sb sandbox.Sandbox, base string) sandbox.Sandbox {
	meta := make(map[string]string, len(sb.Meta)+1)
	for k, v := range sb.Meta {
		meta[k] = v
	}
	meta[MetaBase] = base
	sb.Meta = meta
	return sb
}

// run executes cmd and turns a non-zero exit into an error that names what failed.
func run(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, cmd sandbox.Command, what string) error {
	res, err := p.Exec(ctx, sb, cmd)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s: exit %d: %s", what, res.ExitCode, tail(string(res.Stderr), 20))
	}
	return nil
}

// copyFiles copies a file or directory out of the sandbox and returns its regular files, by path
// relative to it, reading at most limit bytes of contents.
func copyFiles(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, src string, limit int64) (map[string][]byte, error) {
	rc, err := p.CopyOut(ctx, sb, src)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	base := path.Base(src)
	out := map[string][]byte{}
	r := tar.NewReader(rc)
	var total int64
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		total += h.Size
		if total > limit {
			return nil, fmt.Errorf("%s holds more than %d MiB", src, limit>>20)
		}
		body, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if rest, ok := strings.CutPrefix(name, base+"/"); ok {
			name = rest
		}
		out[name] = body
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
