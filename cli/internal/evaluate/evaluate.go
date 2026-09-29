// Package evaluate runs the worker side of an evaluation (docs/specs/evaluations.md, "The run
// pipeline"): the run job seals a case, overlays one side's harness, runs the agent with its caps
// and collects its diff, session log and trace; the verify job applies that diff to a pristine base
// in a separate sandbox with no model key, runs the held-out tests, and checks the case's
// assertions and judge question.
package evaluate

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/alternayte/casebox/cli/internal/agents"
	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/cases"
	"github.com/alternayte/casebox/cli/internal/gitmirror"
	"github.com/alternayte/casebox/cli/internal/recipe"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// Jobs runs the run and verify jobs on this host. Env is the worker's environment, where the
// model keys are; they go into the agent's sandbox and nowhere else. Model is the analysis model
// that answers judge questions; without it the judge answer is null.
type Jobs struct {
	Client      *api.Client
	MirrorRoot  string
	GitHubToken string
	Provider    sandbox.Provider
	// ProviderName names the provider in the sandbox start time the run reports.
	ProviderName string
	Model        *analysis.Client
	Env          map[string]string
}

// deps are what the pipeline reaches outside itself; tests replace the server and the mirrors.
type deps struct {
	provider sandbox.Provider
	name     string
	open     func(ctx context.Context, repo string) (string, error)
	put      func(ctx context.Context, contentType string, data []byte) (string, error)
	get      func(ctx context.Context, hash string) ([]byte, error)
	source   func(ctx context.Context, caseID string) (cases.Source, error)
	env      map[string]string
	model    *analysis.Client
}

func (j Jobs) deps() deps {
	opened := map[string]string{}
	return deps{
		provider: j.Provider,
		name:     j.ProviderName,
		open: func(ctx context.Context, name string) (string, error) {
			if dir, ok := opened[name]; ok {
				return dir, nil
			}
			m, err := gitmirror.Open(ctx, j.MirrorRoot, name, j.GitHubToken)
			if err != nil {
				return "", err
			}
			opened[name] = m.Dir
			return m.Dir, nil
		},
		put: j.Client.PutBlob,
		get: func(ctx context.Context, hash string) ([]byte, error) { return getBlob(ctx, j.Client, hash) },
		source: func(ctx context.Context, caseID string) (cases.Source, error) {
			var src cases.Source
			err := j.Client.Do(ctx, http.MethodGet, "/worker/v1/cases/"+url.PathEscape(caseID)+"/source", nil, &src)
			return src, err
		},
		env:   j.Env,
		model: j.Model,
	}
}

// Available names the agents this worker can run: each agent whose model key its environment
// holds, and the command agent, which needs none.
func Available(env map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, row := range agents.Flags {
		if seen[row.Agent] {
			continue
		}
		a, err := agents.For(agents.Spec{Agent: row.Agent, AgentVersion: row.Min, Model: "m", Harness: "none"})
		if err != nil {
			continue
		}
		if _, err := a.Env(env); err == nil {
			seen[row.Agent] = true
			out = append(out, row.Agent)
		}
	}
	sort.Strings(out)
	return append(out, agents.CommandCLI)
}

// maxBlob is the largest blob the server stores.
const maxBlob = 64 << 20

// getBlob reads a blob with the worker token.
func getBlob(ctx context.Context, c *api.Client, hash string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Server+"/worker/v1/blobs/"+url.PathEscape(hash), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", c.Server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := &api.Error{Status: resp.StatusCode}
		if resp.StatusCode == http.StatusNotFound {
			return nil, worker.Permanent{Err: fmt.Errorf("blob %s: %w", short(hash), err)}
		}
		return nil, err
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBlob+1))
}

// sealedRepo is a sealed repository of a case: at the root of the tree for one repository, under
// its own folder when several are sealed side by side, as validation lays them out.
type sealedRepo struct {
	cases.CaseRepo
	dir    string
	prefix string
}

// openSealed opens the mirrors of the case's sealed repositories and lays them out.
func openSealed(ctx context.Context, d deps, repos []cases.CaseRepo) ([]sealedRepo, error) {
	var sealed []sealedRepo
	for _, r := range repos {
		if r.Role == cases.RoleSealed {
			sealed = append(sealed, sealedRepo{CaseRepo: r})
		}
	}
	if len(sealed) == 0 {
		return nil, worker.Permanent{Err: errors.New("the case has no sealed repository")}
	}
	folders := map[string]bool{}
	for i := range sealed {
		r := &sealed[i]
		dir, err := d.open(ctx, r.Repo)
		if err != nil {
			return nil, err
		}
		if _, err := repo.Resolve(ctx, dir, r.Base); err != nil || strings.HasPrefix(r.Base, "-") {
			return nil, worker.Permanent{Err: fmt.Errorf("%s does not have the case's base commit %s", r.Repo, short(r.Base))}
		}
		r.dir = dir
		if len(sealed) > 1 {
			folder := path.Base(r.Repo)
			if folders[folder] {
				folder = strings.ReplaceAll(r.Repo, "/", "_")
			}
			folders[folder] = true
			r.prefix = folder + "/"
		}
	}
	return sealed, nil
}

// baseTree spools the base trees of the sealed repositories into one tar file, each under its
// prefix, and lists their files with the prefix.
func baseTree(ctx context.Context, sealed []sealedRepo) (*os.File, []string, error) {
	f, err := os.CreateTemp("", "casebox-run-*.tar")
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*os.File, []string, error) {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	tw := tar.NewWriter(f)
	var files []string
	for _, r := range sealed {
		tracked, err := repo.TrackedFiles(ctx, r.dir, r.Base)
		if err != nil {
			return fail(err)
		}
		for _, t := range tracked {
			files = append(files, r.prefix+t)
		}
		a, err := repo.Archive(ctx, r.dir, r.Base)
		if err != nil {
			return fail(err)
		}
		tr := tar.NewReader(a)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				a.Close()
				return fail(fmt.Errorf("read the tree of %s at %s: %w", r.Repo, short(r.Base), err))
			}
			if h.Typeflag == tar.TypeXGlobalHeader {
				continue
			}
			h.Name = r.prefix + h.Name
			if err := tw.WriteHeader(h); err != nil {
				a.Close()
				return fail(err)
			}
			if _, err := io.Copy(tw, tr); err != nil {
				a.Close()
				return fail(err)
			}
		}
		if err := a.Close(); err != nil {
			return fail(err)
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return f, files, nil
}

// envSpec is the recipe's environment with the lockfiles of the base commits.
func envSpec(ctx context.Context, rec repo.Recipe, sealed []sealedRepo, files []string) (sandbox.EnvSpec, error) {
	return recipe.Spec(rec, files, func(f string) ([]byte, error) {
		for _, r := range sealed {
			if rel, ok := strings.CutPrefix(f, r.prefix); ok {
				return repo.ReadAt(ctx, r.dir, r.Base, rel)
			}
		}
		return nil, fmt.Errorf("%s is in no sealed repository", f)
	})
}

// configAt reads casebox.yml at rev; a repository without one gets the defaults.
func configAt(ctx context.Context, dir, rev string) repo.Config {
	var cfg repo.Config
	if data, err := repo.ReadAt(ctx, dir, rev, repo.ConfigPath); err == nil {
		_ = yaml.Unmarshal(data, &cfg)
	}
	return cfg
}

// readOracle downloads and decodes the case's oracle blob.
func readOracle(ctx context.Context, d deps, hash string) (cases.Oracle, error) {
	var o cases.Oracle
	if hash == "" {
		return o, worker.Permanent{Err: errors.New("the job names no oracle")}
	}
	raw, err := d.get(ctx, hash)
	if err != nil {
		return o, fmt.Errorf("read the oracle: %w", err)
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, worker.Permanent{Err: fmt.Errorf("the oracle %s is not JSON: %w", short(hash), err)}
	}
	return o, nil
}

// copyFile copies one regular file out of a sandbox, reading at most limit bytes.
func copyFile(ctx context.Context, p sandbox.Provider, sb sandbox.Sandbox, file string, limit int64) ([]byte, error) {
	rc, err := p.CopyOut(ctx, sb, file)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	r := tar.NewReader(rc)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is not a regular file", file)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > limit {
			return nil, fmt.Errorf("%s holds %d MiB, more than %d MiB", file, h.Size>>20, limit>>20)
		}
		return io.ReadAll(r)
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
