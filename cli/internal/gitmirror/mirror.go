// Package gitmirror keeps bare mirrors of repositories for the worker, with the extra refs Casebox
// reads: pull request heads, Entire checkpoints and git-ai notes. The token comes from the
// worker's environment and is passed per command, never written into the mirror's config.
package gitmirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Refspecs the worker fetches besides branches and tags.
var Refspecs = []string{
	"+refs/heads/*:refs/heads/*",
	"+refs/tags/*:refs/tags/*",
	"+refs/pull/*/head:refs/pull/*/head",
	"+refs/entire/checkpoints/*:refs/entire/checkpoints/*",
	"+refs/notes/ai:refs/notes/ai",
}

// Mirror is one bare repository on disk.
type Mirror struct {
	Dir   string
	URL   string
	token string
}

// Open returns the mirror of repo (host/owner/name) under root, cloning it on first use and
// fetching every Casebox ref.
func Open(ctx context.Context, root, repo, token string) (*Mirror, error) {
	m := &Mirror{Dir: filepath.Join(root, filepath.FromSlash(repo)+".git"), URL: "https://" + repo + ".git", token: token}
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); err != nil {
		if err := os.MkdirAll(m.Dir, 0o700); err != nil {
			return nil, err
		}
		if _, err := m.Git(ctx, "init", "--bare", "--quiet"); err != nil {
			return nil, err
		}
	}
	args := append([]string{"fetch", "--quiet", "--prune", m.URL}, Refspecs...)
	if _, err := m.Git(ctx, args...); err != nil {
		// A repository without pull request refs, Entire refs or notes still fetches; only the
		// missing refs fail.
		if _, err2 := m.Git(ctx, "fetch", "--quiet", "--prune", m.URL, Refspecs[0], Refspecs[1]); err2 != nil {
			return nil, fmt.Errorf("fetch %s: %w", repo, err)
		}
		for _, spec := range Refspecs[2:] {
			_, _ = m.Git(ctx, "fetch", "--quiet", m.URL, spec)
		}
	}
	return m, nil
}

// Fetch fetches refspecs from the repository's remote into the mirror.
func (m *Mirror) Fetch(ctx context.Context, refspecs ...string) error {
	_, err := m.Git(ctx, append([]string{"fetch", "--quiet", m.URL}, refspecs...)...)
	return err
}

// Git runs a git command in the mirror and returns its standard output.
func (m *Mirror) Git(ctx context.Context, args ...string) ([]byte, error) {
	full := []string{"-C", m.Dir, "-c", "credential.helper="}
	if m.token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + m.token))
		full = append(full, "-c", "http.extraHeader=Authorization: Basic "+auth)
	}
	cmd := exec.CommandContext(ctx, "git", append(full, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// Show returns a file from a tree-ish, such as "<commit>:0/metadata.json".
func (m *Mirror) Show(ctx context.Context, object string) ([]byte, error) {
	return m.Git(ctx, "cat-file", "-p", object)
}
