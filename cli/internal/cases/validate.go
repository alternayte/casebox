package cases

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/oracle"
	"github.com/alternayte/casebox/cli/internal/recipe"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/runner"
	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// ValidatePayload is the payload of a case.validate job.
type ValidatePayload struct {
	CaseID     string      `json:"caseId"`
	Kind       string      `json:"kind"`
	Workspace  string      `json:"workspace"`
	Recipe     repo.Recipe `json:"recipe"`
	RecipeHash string      `json:"recipeHash"`
	Repos      []CaseRepo  `json:"repos"`
	Pulls      []Pull      `json:"pulls"`
	Original   *Pull       `json:"original"`
}

// ValidateResult is the answer to a case.validate job.
type ValidateResult struct {
	Passed     bool    `json:"passed"`
	Reason     string  `json:"reason,omitempty"`
	Detail     string  `json:"detail,omitempty"`
	Oracle     string  `json:"oracle,omitempty"`
	FailToPass int     `json:"failToPass"`
	PassToPass int     `json:"passToPass"`
	Seconds    float64 `json:"seconds"`
	Drift      bool    `json:"drift"`
	Weight     float64 `json:"weight"`
	Retire     bool    `json:"retire"`
}

// Oracle is the oracle blob of a validated case (docs/specs/cases.md, "The oracle").
type Oracle struct {
	Kind        string          `json:"kind"`
	Tests       OracleTests     `json:"tests"`
	TestFiles   []string        `json:"testFiles"`
	SourcePatch string          `json:"sourcePatch"`
	TestPatch   string          `json:"testPatch"`
	FixTests    string          `json:"fixTests,omitempty"`
	Assertions  []Assertion     `json:"assertions"`
	Judge       []JudgeQuestion `json:"judge"`
	Commands    []OracleCommand `json:"commands"`
}

// OracleTests are the tests that decide a case.
type OracleTests struct {
	FailToPass []string `json:"failToPass"`
	PassToPass []string `json:"passToPass"`
}

// OracleCommand is a test command and the format of its results; a command without results
// counts pass or fail by its exit code only.
type OracleCommand struct {
	Command string `json:"command"`
	Results string `json:"results,omitempty"`
}

// Assertion is a steering assertion: a forbidden file change, a command that must run before the
// agent says done, or a pattern the diff must or must not match.
type Assertion struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// JudgeQuestion is a narrow question a judge model answers about the agent's work.
type JudgeQuestion struct {
	Question string `json:"question"`
}

// uploader puts a blob on the server and returns its hash.
type uploader func(ctx context.Context, contentType string, data []byte) (string, error)

// Validate runs a case.validate job in this host's sandbox provider.
func (j Jobs) Validate(ctx context.Context, job worker.Job) (any, error) {
	var p ValidatePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.CaseID == "" || len(p.Repos) == 0 {
		return nil, worker.Permanent{Err: errors.New("the job names no case or repository")}
	}
	if err := p.Recipe.Validate(); err != nil {
		return nil, worker.Permanent{Err: err}
	}
	return validate(ctx, j.Provider, j.mirrors(), j.Client.PutBlob, p)
}

// layoutRepo is a sealed repository in the verifier's tree: at the root for one repository,
// under its own folder when several are sealed side by side.
type layoutRepo struct {
	CaseRepo
	dir    string
	prefix string
	cfg    repo.Config
}

func validate(ctx context.Context, p sandbox.Provider, open source, upload uploader, pl ValidatePayload) (ValidateResult, error) {
	var sealed []layoutRepo
	for _, r := range pl.Repos {
		if r.Role == RoleSealed {
			sealed = append(sealed, layoutRepo{CaseRepo: r})
		}
	}
	if len(sealed) == 0 {
		return ValidateResult{}, worker.Permanent{Err: errors.New("the case has no sealed repository")}
	}
	folders := map[string]bool{}
	for i := range sealed {
		r := &sealed[i]
		dir, err := open(ctx, r.Repo)
		if err != nil {
			return ValidateResult{}, err
		}
		if !hasCommit(ctx, dir, r.Base) || (r.Merged != nil && !hasCommit(ctx, dir, *r.Merged)) {
			return ValidateResult{}, worker.Permanent{Err: fmt.Errorf("%s does not have the case's commits", r.Repo)}
		}
		r.dir, r.cfg = dir, config(ctx, dir)
		if len(sealed) > 1 {
			folder := path.Base(r.Repo)
			if folders[folder] {
				folder = strings.ReplaceAll(r.Repo, "/", "_")
			}
			folders[folder] = true
			r.prefix = folder + "/"
		}
	}

	v := &validation{provider: p, commands: pl.Recipe.Test, egress: Registries(pl.Recipe)}
	if err := runner.CheckEgress(v.egress); err != nil {
		return ValidateResult{}, worker.Permanent{Err: err}
	}

	// The change, split into the source patch and the test patch.
	var change Change
	hasMerged := false
	for _, r := range sealed {
		if r.Merged == nil {
			continue
		}
		hasMerged = true
		d, err := diff(ctx, r.dir, r.Base, *r.Merged, r.prefix)
		if err != nil {
			return ValidateResult{}, err
		}
		c, err := splitChange(d, r.prefix, r.cfg.TestGlobs())
		if err != nil {
			return ValidateResult{}, err
		}
		change.Source = append(change.Source, c.Source...)
		change.Tests = append(change.Tests, c.Tests...)
		change.TestFiles = append(change.TestFiles, c.TestFiles...)
		change.SourceFiles = append(change.SourceFiles, c.SourceFiles...)
		change.Dirs = append(change.Dirs, c.Dirs...)
	}

	if !hasMerged && pl.Kind != Steering {
		return ValidateResult{}, worker.Permanent{Err: fmt.Errorf("a %s case needs its merged commit", pl.Kind)}
	}

	tree, files, err := baseTree(ctx, sealed)
	if err != nil {
		return ValidateResult{}, err
	}
	defer os.Remove(tree.Name())
	defer tree.Close()
	spec, err := specAt(ctx, pl.Recipe, sealed, files)
	if err != nil {
		return ValidateResult{}, err
	}
	if failed, err := v.prepare(ctx, spec); err != nil {
		return ValidateResult{}, err
	} else if failed != nil {
		return *failed, nil
	}

	oracleOut := Oracle{Kind: pl.Kind, Tests: OracleTests{FailToPass: []string{}, PassToPass: []string{}}, TestFiles: nonNil(change.TestFiles),
		Assertions: []Assertion{}, Judge: []JudgeQuestion{}}
	for _, c := range pl.Recipe.Test {
		oracleOut.Commands = append(oracleOut.Commands, OracleCommand{Command: c.Command, Results: c.Results})
	}

	var seconds float64
	if !hasMerged {
		// A steering case without a pull request: the base must build and its tests run.
		base, err := v.run(ctx, spec, tree, nil)
		if err != nil {
			return ValidateResult{}, err
		}
		switch {
		case base.TimedOut || base.Duration >= MaxRunTime:
			return v.failed(ReasonTooSlow, fmt.Sprintf("the run at the base took %s or hit a timeout\n%s", base.Duration.Round(time.Second), base.Output)), nil
		case base.Run.Error != "":
			return v.failed(ReasonBuild, "the tests do not build or run at the base: "+base.Run.Error), nil
		}
		seconds = base.Duration.Seconds()
	} else {
		testPatch := runner.Patch{Name: "tests", Diff: change.Tests}
		base, err := v.run(ctx, spec, tree, []runner.Patch{testPatch})
		if err != nil {
			return ValidateResult{}, err
		}
		if !base.Applied {
			return v.failed(ReasonApplyFailed, "the test patch does not apply at the base: "+base.ApplyOutput), nil
		}
		var merged []runOutcome
		for range 3 {
			m, err := v.run(ctx, spec, tree, []runner.Patch{{Name: "source", Diff: change.Source}, testPatch})
			if err != nil {
				return ValidateResult{}, err
			}
			if !m.Applied {
				return v.failed(ReasonApplyFailed, fmt.Sprintf("the %s patch does not apply at the base: %s", m.FailedPatch, m.ApplyOutput)), nil
			}
			merged = append(merged, m)
		}
		dirs := change.Dirs
		if pl.Kind == Regression && pl.Original != nil {
			// The original pull request's tests are part of the oracle: its packages are in scope.
			od, err := originalDirs(ctx, sealed[0], *pl.Original)
			if err != nil {
				return ValidateResult{}, err
			}
			dirs = append(dirs, od...)
		}
		verdict := decide(pl.Kind, base, merged, scopeOf(dirs))
		if verdict.Reason != "" {
			return v.failed(verdict.Reason, verdict.Detail), nil
		}
		oracleOut.Tests = OracleTests{FailToPass: nonNil(verdict.FailToPass), PassToPass: nonNil(verdict.PassToPass)}
		seconds = verdict.Slowest.Seconds()

		if pl.Kind == Regression {
			if pl.Original == nil {
				return ValidateResult{}, worker.Permanent{Err: errors.New("a regression case names no original pull request")}
			}
			failed, err := v.originalFails(ctx, pl.Recipe, sealed[0], *pl.Original, change.Tests, verdict.FailToPass)
			if err != nil {
				return ValidateResult{}, err
			}
			if failed != nil {
				return *failed, nil
			}
		}
	}

	d, err := driftOf(ctx, sealed)
	if err != nil {
		return ValidateResult{}, err
	}

	if len(change.Source) > 0 {
		if oracleOut.SourcePatch, err = upload(ctx, "text/x-diff", change.Source); err != nil {
			return ValidateResult{}, fmt.Errorf("upload the source patch: %w", err)
		}
	}
	if len(change.Tests) > 0 {
		if oracleOut.TestPatch, err = upload(ctx, "text/x-diff", change.Tests); err != nil {
			return ValidateResult{}, fmt.Errorf("upload the test patch: %w", err)
		}
		if pl.Kind == Regression {
			oracleOut.FixTests = oracleOut.TestPatch
		}
	}
	body, err := json.Marshal(oracleOut)
	if err != nil {
		return ValidateResult{}, err
	}
	hash, err := upload(ctx, "application/json", body)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("upload the oracle: %w", err)
	}
	weight := 1.0
	if d.drift {
		weight = 0.5
	}
	return ValidateResult{
		Passed:     true,
		Detail:     v.detail(d),
		Oracle:     hash,
		FailToPass: len(oracleOut.Tests.FailToPass),
		PassToPass: len(oracleOut.Tests.PassToPass),
		Seconds:    seconds,
		Drift:      d.drift,
		Weight:     weight,
		Retire:     d.retire(),
	}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// validation runs the verifier with one recipe's test commands and egress allow-list.
type validation struct {
	provider sandbox.Provider
	commands []repo.TestCommand
	egress   []string
	notes    []string
}

// prepare builds the environment; a build that fails is the case's failure (reason build).
func (v *validation) prepare(ctx context.Context, spec sandbox.EnvSpec) (*ValidateResult, error) {
	if _, err := v.provider.Prepare(ctx, spec); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		r := v.failed(ReasonBuild, "the environment does not build at the base: "+err.Error())
		return &r, nil
	}
	return nil, nil
}

// run applies patches to the base tree in a fresh verifier and runs the test commands. When the
// provider cannot honour the egress allow-list, it runs with no network and says so.
func (v *validation) run(ctx context.Context, spec sandbox.EnvSpec, tree *os.File, patches []runner.Patch) (runOutcome, error) {
	if _, err := tree.Seek(0, io.SeekStart); err != nil {
		return runOutcome{}, err
	}
	res, err := runner.ApplyAndRun(ctx, v.provider, spec, tree, patches, v.commands, v.egress)
	if err != nil && len(v.egress) > 0 && errors.Is(err, sandbox.ErrUnsupported) {
		v.notes = append(v.notes, "The sandbox provider has no egress allow-list, so the tests ran with no network: "+err.Error())
		v.egress = nil
		if _, err := tree.Seek(0, io.SeekStart); err != nil {
			return runOutcome{}, err
		}
		res, err = runner.ApplyAndRun(ctx, v.provider, spec, tree, patches, v.commands, nil)
	}
	if err != nil {
		return runOutcome{}, err
	}
	return outcome(res)
}

func (v *validation) failed(reason, detail string) ValidateResult {
	for _, n := range v.notes {
		detail += "\n" + n
	}
	return ValidateResult{Passed: false, Reason: reason, Detail: clip(detail, 4000)}
}

func (v *validation) detail(d driftResult) string {
	var parts []string
	if len(v.egress) > 0 {
		parts = append(parts, "Egress: "+strings.Join(v.egress, ", ")+".")
	}
	parts = append(parts, v.notes...)
	if len(d.missing) > 0 {
		parts = append(parts, fmt.Sprintf("Harness drift: %d of %d references are missing at the base: %s.", len(d.missing), d.refs, list(d.missing, 20)))
	}
	return clip(strings.Join(parts, "\n"), 4000)
}

// originalFails checks a regression case: the original change alone, with the fix's tests, must
// fail at least one of the fix's fail-to-pass tests. It returns a failed result when it does not.
func (v *validation) originalFails(ctx context.Context, rec repo.Recipe, r layoutRepo, original Pull, fixTests []byte, failToPass []string) (*ValidateResult, error) {
	if !hasCommit(ctx, r.dir, original.MergeSha) {
		return nil, worker.Permanent{Err: fmt.Errorf("%s does not have the original pull request's commit %s", r.Repo, short(original.MergeSha))}
	}
	base, err := firstParent(ctx, r.dir, original.MergeSha)
	if err != nil {
		return nil, worker.Permanent{Err: err}
	}
	at := r
	at.Base = base
	tree, files, err := baseTree(ctx, []layoutRepo{at})
	if err != nil {
		return nil, err
	}
	defer os.Remove(tree.Name())
	defer tree.Close()
	spec, err := specAt(ctx, rec, []layoutRepo{at}, files)
	if err != nil {
		return nil, err
	}
	if failed, err := v.prepare(ctx, spec); failed != nil || err != nil {
		return failed, err
	}
	change, err := diff(ctx, r.dir, base, original.MergeSha, r.prefix)
	if err != nil {
		return nil, err
	}
	o, err := v.run(ctx, spec, tree, []runner.Patch{{Name: "original", Diff: change}, {Name: "tests", Diff: fixTests}})
	if err != nil {
		return nil, err
	}
	if !o.Applied {
		f := v.failed(ReasonApplyFailed, fmt.Sprintf("the %s patch does not apply to the original pull request's base: %s", o.FailedPatch, o.ApplyOutput))
		return &f, nil
	}
	results := o.Run.ByID()
	for _, id := range failToPass {
		if results[id].Status != oracle.Passed {
			return nil, nil
		}
	}
	f := v.failed(ReasonFixDoesNotFail, fmt.Sprintf("with the original change alone, every one of the fix's %d fail-to-pass tests passes", len(failToPass)))
	return &f, nil
}

// originalDirs are the directories the original pull request changed.
func originalDirs(ctx context.Context, r layoutRepo, original Pull) ([]string, error) {
	if !hasCommit(ctx, r.dir, original.MergeSha) {
		return nil, nil
	}
	base, err := firstParent(ctx, r.dir, original.MergeSha)
	if err != nil {
		return nil, nil
	}
	files, err := changedFiles(ctx, r.dir, base, original.MergeSha)
	if err != nil {
		return nil, err
	}
	dirs := map[string]bool{}
	for _, f := range files {
		dirs[path.Dir(f)] = true
	}
	return sortedKeys(dirs), nil
}

// baseTree spools the base trees of the sealed repositories into one tar file, each under its
// prefix, and lists its files with their prefix.
func baseTree(ctx context.Context, sealed []layoutRepo) (*os.File, []string, error) {
	f, err := os.CreateTemp("", "casebox-case-*.tar")
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
	return f, files, nil
}

// specAt is the recipe's environment with the lockfiles of the base commits.
func specAt(ctx context.Context, rec repo.Recipe, sealed []layoutRepo, files []string) (sandbox.EnvSpec, error) {
	return recipe.Spec(rec, files, func(f string) ([]byte, error) {
		for _, r := range sealed {
			if rel, ok := strings.CutPrefix(f, r.prefix); ok {
				return repo.ReadAt(ctx, r.dir, r.Base, rel)
			}
		}
		return nil, fmt.Errorf("%s is in no sealed repository", f)
	})
}

// driftResult is the harness drift of a case's sealed repositories.
type driftResult struct {
	drift   bool
	refs    int
	missing []string
}

// retire is true when more than half of the harness's references are missing at the base.
func (d driftResult) retire() bool { return d.refs > 0 && 2*len(d.missing) > d.refs }

// driftOf checks the current harness (at the default branch) against each sealed base.
func driftOf(ctx context.Context, sealed []layoutRepo) (driftResult, error) {
	var out driftResult
	for _, r := range sealed {
		h, err := repo.Harness(ctx, r.dir, "HEAD", r.cfg.HarnessGlobs())
		if err != nil {
			return driftResult{}, err
		}
		harness := map[string][]byte{}
		for _, f := range h.Files {
			body, err := repo.ReadAt(ctx, r.dir, "HEAD", f)
			if err != nil {
				return driftResult{}, fmt.Errorf("read %s: %w", f, err)
			}
			harness[f] = body
		}
		files, err := repo.TrackedFiles(ctx, r.dir, r.Base)
		if err != nil {
			return driftResult{}, err
		}
		var just, targets, scripts []string
		for _, f := range files {
			name := path.Base(f)
			switch {
			case name == "justfile" || name == "Justfile" || name == ".justfile":
				if body, err := repo.ReadAt(ctx, r.dir, r.Base, f); err == nil {
					just = append(just, runner.JustRecipes(body)...)
				}
			case name == "Makefile" || name == "makefile" || name == "GNUmakefile":
				if body, err := repo.ReadAt(ctx, r.dir, r.Base, f); err == nil {
					targets = append(targets, runner.MakeTargets(body)...)
				}
			case name == "package.json" && !strings.Contains("/"+f, "/node_modules/"):
				if body, err := repo.ReadAt(ctx, r.dir, r.Base, f); err == nil {
					if s, err := runner.PackageScripts(body); err == nil {
						scripts = append(scripts, s...)
					}
				}
			}
		}
		drift, refs, missing := runner.Drift(harness, files, just, targets, scripts)
		out.drift = out.drift || drift
		out.refs += len(refs)
		for _, m := range missing {
			out.missing = append(out.missing, r.prefix+m)
		}
	}
	return out, nil
}

// registries are the package registries of each language, which the verifier may reach.
var registries = []struct {
	pattern *regexp.Regexp
	hosts   []string
}{
	{regexp.MustCompile(`(^|[^\w.-])(go (test|build|vet|run|mod|generate|install)\b|gotestsum\b|golang:)`), []string{"proxy.golang.org", "sum.golang.org"}},
	{regexp.MustCompile(`(^|[^\w.-])(dotnet\b|mcr\.microsoft\.com/dotnet)`), []string{"api.nuget.org", "*.nuget.org"}},
	{regexp.MustCompile(`(^|[^\w.-])(npm|npx|bun|bunx|pnpm|yarn|node|jest|vitest|oven/bun)\b`), []string{"registry.npmjs.org", "registry.yarnpkg.com"}},
	{regexp.MustCompile(`(^|[^\w.-])(python3?|pytest|pip3?|uv|poetry|tox|nox)\b`), []string{"pypi.org", "files.pythonhosted.org"}},
	{regexp.MustCompile(`(^|[^\w.-])(mvn|mvnw|gradle|gradlew|maven|eclipse-temurin)\b`), []string{"repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org", "services.gradle.org"}},
}

// Registries is the verifier's egress allow-list: the package registries of the languages the
// recipe's image, install steps and test commands use.
func Registries(r repo.Recipe) []string {
	text := []string{r.Image}
	text = append(text, r.Install...)
	for _, t := range r.Test {
		text = append(text, t.Command)
	}
	joined := strings.Join(text, "\n")
	var out []string
	for _, reg := range registries {
		if reg.pattern.MatchString(joined) {
			out = append(out, reg.hosts...)
		}
	}
	return out
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
