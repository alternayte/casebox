// Package gitmirror keeps bare mirrors of repositories for the worker, with the extra refs Casebox
// reads: pull request refs, Entire checkpoints and git-ai notes. The tokens come from the
// worker's environment and are passed per command, never written into the mirror's config.
package gitmirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alternayte/casebox/cli/internal/api"
)

// Code hosts, as the server names them.
const (
	GitHub      = "github"
	AzureDevOps = "azure-devops"
)

// Remote is where one repository is fetched from, with the worker's token for its host.
type Remote struct {
	URL   string
	Host  string
	Token string
}

// PullRef is the ref that holds a pull request's commits: its head on GitHub, and on Azure DevOps
// its merge commit, whose second parent is the head.
func (r Remote) PullRef(number int) string {
	if r.Host == AzureDevOps {
		return fmt.Sprintf("refs/pull/%d/merge", number)
	}
	return fmt.Sprintf("refs/pull/%d/head", number)
}

// refspecs the worker fetches: branches and tags, then the refs a repository may lack.
func (r Remote) refspecs() []string {
	pulls := "+refs/pull/*/head:refs/pull/*/head"
	if r.Host == AzureDevOps {
		pulls = "+refs/pull/*/merge:refs/pull/*/merge"
	}
	return []string{
		"+refs/heads/*:refs/heads/*",
		"+refs/tags/*:refs/tags/*",
		pulls,
		"+refs/entire/checkpoints/*:refs/entire/checkpoints/*",
		"+refs/notes/ai:refs/notes/ai",
	}
}

// Remotes finds each repository's clone URL in the server's list of workspace repositories, and
// pairs it with the worker's token for that host: GITHUB_TOKEN, or AZURE_DEVOPS_TOKEN, a personal
// access token with Code (Read).
type Remotes struct {
	Client           *api.Client
	GitHubToken      string
	AzureDevOpsToken string

	mu    sync.Mutex
	known map[string]Remote
}

// Remote returns where repo is fetched from. The list is read again when it lacks repo, so a
// repository added after the worker started is found; a repository in no workspace is GitHub's.
func (r *Remotes) Remote(ctx context.Context, repo string) (Remote, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if remote, ok := r.known[repo]; ok {
		return remote, nil
	}
	var listed []struct {
		Repo     string `json:"repo"`
		Host     string `json:"host"`
		CloneURL string `json:"cloneUrl"`
	}
	if err := r.Client.Do(ctx, http.MethodGet, "/worker/v1/repos", nil, &listed); err != nil {
		return Remote{}, fmt.Errorf("list the workspace repositories: %w", err)
	}
	r.known = map[string]Remote{}
	for _, l := range listed {
		token := r.GitHubToken
		if l.Host == AzureDevOps {
			token = r.AzureDevOpsToken
		}
		r.known[l.Repo] = Remote{URL: l.CloneURL, Host: l.Host, Token: token}
	}
	if remote, ok := r.known[repo]; ok {
		return remote, nil
	}
	return Remote{URL: "https://" + repo + ".git", Host: GitHub, Token: r.GitHubToken}, nil
}

// Mirror is one bare repository on disk.
type Mirror struct {
	Dir    string
	URL    string
	Remote Remote
}

// Open returns the mirror of repo (host/owner/name) under root, cloning it on first use and
// fetching every Casebox ref.
func Open(ctx context.Context, root, repo string, remotes *Remotes) (*Mirror, error) {
	remote, err := remotes.Remote(ctx, repo)
	if err != nil {
		return nil, err
	}
	if remote.Host == AzureDevOps && remote.Token == "" {
		return nil, fmt.Errorf("%s is on Azure DevOps Server; set AZURE_DEVOPS_TOKEN to a personal access token with Code (Read) before casebox worker", repo)
	}
	m := &Mirror{Dir: filepath.Join(root, filepath.FromSlash(repo)+".git"), URL: remote.URL, Remote: remote}
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); err != nil {
		if err := os.MkdirAll(m.Dir, 0o700); err != nil {
			return nil, err
		}
		if _, err := m.Git(ctx, "init", "--bare", "--quiet"); err != nil {
			return nil, err
		}
	}
	refspecs := remote.refspecs()
	args := append([]string{"fetch", "--quiet", "--prune", m.URL}, refspecs...)
	if _, err := m.Git(ctx, args...); err != nil {
		// A repository without pull request refs, Entire refs or notes still fetches; only the
		// missing refs fail.
		if _, err2 := m.Git(ctx, "fetch", "--quiet", "--prune", m.URL, refspecs[0], refspecs[1]); err2 != nil {
			return nil, fmt.Errorf("fetch %s: %w", repo, err)
		}
		for _, spec := range refspecs[2:] {
			_, _ = m.Git(ctx, "fetch", "--quiet", m.URL, spec)
		}
	}
	m.followRemoteHead(ctx)
	return m, nil
}

// followRemoteHead points the mirror's HEAD at the remote's default branch, so HEAD in the mirror
// is the default branch as on the host. When the remote does not say, HEAD stays as it is.
func (m *Mirror) followRemoteHead(ctx context.Context) {
	out, err := m.Git(ctx, "ls-remote", "--symref", m.URL, "HEAD")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		ref, rest, ok := strings.Cut(strings.TrimPrefix(line, "ref: "), "\t")
		if !ok || !strings.HasPrefix(line, "ref: ") || strings.TrimSpace(rest) != "HEAD" || !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		if _, err := m.Git(ctx, "rev-parse", "--verify", "--quiet", ref); err == nil {
			_, _ = m.Git(ctx, "symbolic-ref", "HEAD", ref)
		}
		return
	}
}

// Fetch fetches refspecs from the repository's remote into the mirror.
func (m *Mirror) Fetch(ctx context.Context, refspecs ...string) error {
	_, err := m.Git(ctx, append([]string{"fetch", "--quiet", m.URL}, refspecs...)...)
	return err
}

// Git runs a git command in the mirror and returns its standard output.
func (m *Mirror) Git(ctx context.Context, args ...string) ([]byte, error) {
	full := []string{"-C", m.Dir, "-c", "credential.helper="}
	// GitHub takes any user name with a token, and Azure DevOps with a personal access token.
	if m.Remote.Token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + m.Remote.Token))
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
