// Package harbor writes an approved case as a Harbor task (harborframework.com, task format
// schema 1.4; docs/specs/cases.md, "Harbor export"). A task holds no dependency on Casebox:
//
//   - instruction.md: the approved instruction, with the interface signatures under
//     "Interfaces your solution must provide".
//   - task.toml: the task's name, metadata, timeouts and environment.
//   - environment/Dockerfile: a build stage that compiles the verdict program (verdict/ and the
//     oracle package's parsers), then the Docker provider's build of the recipe, then the base
//     tree (environment/base/, without the held-out test files) committed as the one commit of a
//     fresh repository, as the runner seals it. environment/docker-compose.yaml adds the
//     recipe's services as sidecars.
//   - tests/test.sh: restores the held-out test files as they are at the base (tests/base/),
//     applies tests/tests.patch, runs the recipe's test commands and has the verdict program
//     decide from their result files and tests/oracle.json.
//   - solution/solve.sh and solution/solution.patch: the merged source change.
package harbor

import (
	"archive/tar"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/alternayte/casebox/cli/internal/oracle"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/docker"
	"github.com/alternayte/casebox/cli/internal/sandbox/egressproxy"
)

//go:embed verdict/main.go
var verdictSource embed.FS

// Task timeouts and resources (the spec's Harbor section).
const (
	agentTimeout       = 1800
	minVerifierTimeout = 600
	cpus               = 2
	memoryMB           = 4096
)

// verdictBinary is where the environment puts the verdict program.
const verdictBinary = "/usr/local/bin/casebox-verdict"

// oracleImport is the oracle package's import path, which the exported verdict program replaces
// with its own module's.
const oracleImport = "github.com/alternayte/casebox/cli/internal/oracle"

// Case is what the export needs of an approved case.
type Case struct {
	ID          string
	Workspace   string
	Kind        string
	Scope       string
	Source      string
	WorkItem    string
	FailToPass  int
	PassToPass  int
	Drift       bool
	Weight      float64
	Seconds     float64 // the slowest validation run
	Instruction string
	Signatures  []string
}

// Oracle is the part of the oracle JSON the export reads.
type Oracle struct {
	Kind  string `json:"kind"`
	Tests struct {
		FailToPass []string `json:"failToPass"`
		PassToPass []string `json:"passToPass"`
	} `json:"tests"`
	TestFiles   []string `json:"testFiles"`
	SourcePatch string   `json:"sourcePatch"`
	TestPatch   string   `json:"testPatch"`
}

// Task is one case with everything its Harbor task is made of.
type Task struct {
	Case        Case
	Oracle      Oracle
	Recipe      repo.Recipe
	Env         sandbox.EnvSpec // the recipe's environment with the base commit's lockfiles
	Base        io.Reader       // the base tree, a tar stream (git archive of the base commit)
	SourcePatch []byte
	TestPatch   []byte
}

// Name is the task's directory name and the part of its Harbor name after "casebox/".
func (t Task) Name() string {
	return t.Case.Workspace + "-" + t.Case.ID
}

// Write writes the task into dir, replacing what dir held.
func Write(dir string, t Task) error {
	if t.Case.Instruction == "" {
		return errors.New("the case has no instruction")
	}
	if err := t.Recipe.Validate(); err != nil {
		return fmt.Errorf("the recipe: %w", err)
	}
	heldOut := map[string]bool{}
	for _, f := range t.Oracle.TestFiles {
		clean, err := cleanPath(f)
		if err != nil {
			return fmt.Errorf("held-out file: %w", err)
		}
		heldOut[clean] = true
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	w := writer{dir: dir}
	w.file("instruction.md", instruction(t.Case), 0o644)
	w.file("task.toml", taskTOML(t), 0o644)

	files, atBase, err := w.base(t.Base, heldOut)
	if err != nil {
		return err
	}
	w.file("environment/base.files", nulList(files), 0o644)
	for name, body := range t.Env.Context {
		clean, err := cleanPath(name)
		if err != nil {
			return fmt.Errorf("lockfile: %w", err)
		}
		w.file("environment/files/"+clean, body, 0o644)
	}
	dockerfile, err := Dockerfile(t.Env, sortedKeys(heldOut))
	if err != nil {
		return err
	}
	w.file("environment/Dockerfile", []byte(dockerfile), 0o644)
	if compose, err := composeFile(t.Env.Services); err != nil {
		return err
	} else if compose != nil {
		w.file("environment/docker-compose.yaml", compose, 0o644)
	}
	if err := w.verdict(); err != nil {
		return err
	}

	oracleJSON, err := json.MarshalIndent(map[string][]string{
		"failToPass": nonNil(t.Oracle.Tests.FailToPass),
		"passToPass": nonNil(t.Oracle.Tests.PassToPass),
	}, "", "  ")
	if err != nil {
		return err
	}
	w.file("tests/oracle.json", append(oracleJSON, '\n'), 0o644)
	w.file("tests/tests.patch", t.TestPatch, 0o644)
	w.file("tests/test.sh", []byte(TestScript(t.Env.Dir(), sortedKeys(heldOut), atBase, t.Recipe.Test)), 0o755)
	w.file("solution/solution.patch", t.SourcePatch, 0o644)
	w.file("solution/solve.sh", []byte(solveScript(t.Env.Dir())), 0o755)
	return w.err
}

type writer struct {
	dir string
	err error
}

func (w *writer) file(name string, body []byte, mode os.FileMode) {
	if w.err != nil {
		return
	}
	target := filepath.Join(w.dir, filepath.FromSlash(name))
	if w.err = os.MkdirAll(filepath.Dir(target), 0o755); w.err != nil {
		return
	}
	w.err = os.WriteFile(target, body, mode)
	if w.err == nil {
		// WriteFile keeps an existing file's mode and applies the umask; the task's modes are fixed.
		w.err = os.Chmod(target, mode)
	}
}

// base writes the base tree into environment/base/ without the held-out files, .casebox/, any
// .git and the archive's pax header, and the held-out files that exist at the base into
// tests/base/. It returns the paths in environment/base/, sorted, and whether tests/base holds
// anything.
func (w *writer) base(tree io.Reader, heldOut map[string]bool) ([]string, bool, error) {
	if w.err != nil {
		return nil, false, w.err
	}
	if tree == nil {
		return nil, false, errors.New("no base tree")
	}
	r := tar.NewReader(tree)
	var files []string
	atBase := false
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("read the base tree: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeSymlink:
		default:
			continue
		}
		name, err := cleanPath(strings.TrimSuffix(h.Name, "/"))
		if err != nil {
			return nil, false, fmt.Errorf("the base tree: %w", err)
		}
		if skipped(name) {
			continue
		}
		root := "environment/base/"
		if held(name, heldOut) {
			root, atBase = "tests/base/", true
		} else {
			files = append(files, name)
		}
		target := filepath.Join(w.dir, filepath.FromSlash(root+name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, false, err
		}
		if h.Typeflag == tar.TypeSymlink {
			if err := os.Symlink(h.Linkname, target); err != nil {
				return nil, false, fmt.Errorf("the base tree's symlink %s: %w", name, err)
			}
			continue
		}
		body, err := io.ReadAll(r)
		if err != nil {
			return nil, false, fmt.Errorf("read the base tree: %w", err)
		}
		mode := os.FileMode(0o644)
		if h.Mode&0o111 != 0 {
			mode = 0o755
		}
		w.file(root+name, body, mode)
		if w.err != nil {
			return nil, false, w.err
		}
	}
	sort.Strings(files)
	return files, atBase, nil
}

// verdict writes the verdict program's module into environment/verdict/: its main package and
// the oracle package's source without its tests.
func (w *writer) verdict() error {
	main, err := verdictSource.ReadFile("verdict/main.go")
	if err != nil {
		return err
	}
	w.file("environment/verdict/go.mod", []byte("module casebox-verdict\n\ngo 1.26\n"), 0o644)
	w.file("environment/verdict/main.go", bytes.Replace(main, []byte(strconv.Quote(oracleImport)), []byte(`"casebox-verdict/oracle"`), 1), 0o644)
	return fs.WalkDir(oracle.Source, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, "_test.go") || !strings.HasSuffix(p, ".go") {
			return err
		}
		body, err := oracle.Source.ReadFile(p)
		if err != nil {
			return err
		}
		w.file("environment/verdict/oracle/"+p, body, 0o644)
		return w.err
	})
}

// skipped is what never enters a sealed tree: .casebox/ and any .git.
func skipped(name string) bool {
	parts := strings.Split(name, "/")
	if parts[0] == ".casebox" {
		return true
	}
	for _, p := range parts {
		if p == ".git" {
			return true
		}
	}
	return false
}

// held reports whether name is a held-out file or lies under a held-out directory.
func held(name string, heldOut map[string]bool) bool {
	parts := strings.Split(name, "/")
	for i := range parts {
		if heldOut[strings.Join(parts[:i+1], "/")] {
			return true
		}
	}
	return false
}

func instruction(c Case) []byte {
	text := strings.TrimSpace(c.Instruction) + "\n"
	if len(c.Signatures) > 0 {
		text += "\n## Interfaces your solution must provide\n\n```\n" + strings.Join(c.Signatures, "\n") + "\n```\n"
	}
	return []byte(text)
}

// taskTOML is task.toml, written by hand: the export needs strings, integers, floats, booleans
// and string arrays only.
func taskTOML(t Task) []byte {
	c := t.Case
	var b strings.Builder
	kv := func(key, value string) { b.WriteString(key + " = " + value + "\n") }
	kv("schema_version", tomlString("1.4"))
	b.WriteString("\n[task]\n")
	kv("name", tomlString("casebox/"+t.Name()))
	kv("description", tomlString(fmt.Sprintf("Casebox %s case %s of workspace %s, from %s.", c.Kind, c.ID, c.Workspace, c.Source)))
	kv("keywords", tomlStrings([]string{c.Kind, c.Scope}))
	b.WriteString("\n[metadata]\n")
	kv("source", tomlString(c.Source))
	if c.WorkItem != "" {
		kv("work_item", tomlString(c.WorkItem))
	}
	kv("fail_to_pass", strconv.Itoa(c.FailToPass))
	kv("pass_to_pass", strconv.Itoa(c.PassToPass))
	kv("drift", strconv.FormatBool(c.Drift))
	kv("weight", tomlFloat(c.Weight))
	b.WriteString("\n[verifier]\n")
	kv("timeout_sec", strconv.Itoa(VerifierTimeout(c.Seconds)))
	b.WriteString("\n[agent]\n")
	kv("timeout_sec", strconv.Itoa(agentTimeout))
	b.WriteString("\n[environment]\n")
	if hosts := Registries(t.Recipe); len(hosts) > 0 {
		kv("network_mode", tomlString("allowlist"))
		kv("allowed_hosts", tomlStrings(hosts))
	} else {
		// Harbor refuses an empty allow-list; a recipe without a known registry reaches nothing.
		kv("network_mode", tomlString("no-network"))
	}
	kv("cpus", strconv.Itoa(cpus))
	kv("memory_mb", strconv.Itoa(memoryMB))
	return []byte(b.String())
}

// VerifierTimeout is three times the slowest validation run, at least 600 seconds.
func VerifierTimeout(seconds float64) int {
	return max(minVerifierTimeout, int(math.Ceil(3*seconds)))
}

func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlStrings(list []string) string {
	quoted := make([]string, len(list))
	for i, s := range list {
		quoted[i] = tomlString(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// tomlFloat always has a fraction, so TOML reads a float.
func tomlFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// registries maps the words of a recipe that name a language to its package registries.
var registries = []struct {
	words    []string
	suffixes []string
	hosts    []string
}{
	{words: []string{"golang", "go", "go.mod", "go.sum", "go-test-json"}, hosts: []string{"proxy.golang.org", "sum.golang.org"}},
	{words: []string{"dotnet", "nuget", "trx", "packages.lock.json"}, suffixes: []string{".csproj", ".fsproj", ".sln", ".slnx"}, hosts: []string{"api.nuget.org"}},
	{words: []string{"node", "npm", "npx", "pnpm", "yarn", "bun", "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb"}, hosts: []string{"registry.npmjs.org", "registry.yarnpkg.com"}},
	{words: []string{"python", "python3", "pip", "pip3", "pytest", "poetry", "uv", "uv.lock", "poetry.lock", "requirements.txt", "pyproject.toml"}, hosts: []string{"pypi.org", "files.pythonhosted.org"}},
	{words: []string{"java", "maven", "mvn", "mvnw", "gradle", "gradlew", "pom.xml", "build.gradle", "build.gradle.kts", "openjdk", "eclipse-temurin", "temurin"}, hosts: []string{"repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org", "services.gradle.org"}},
	{words: []string{"rust", "cargo", "cargo.lock"}, hosts: []string{"crates.io", "index.crates.io", "static.crates.io"}},
}

// Registries are the package registries of the recipe's languages, which the agent and the
// verifier may reach. The languages come from the words of the recipe's image, install steps,
// test commands and lockfiles.
func Registries(r repo.Recipe) []string {
	parts := append([]string{r.Image}, r.Install...)
	for _, t := range r.Test {
		parts = append(parts, t.Command, t.Results)
	}
	parts = append(parts, r.Lockfiles...)
	tokens := map[string]bool{}
	for _, part := range parts {
		for _, tok := range strings.FieldsFunc(strings.ToLower(part), func(c rune) bool {
			return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_')
		}) {
			tokens[strings.Trim(tok, ".")] = true
		}
	}
	seen := map[string]bool{}
	var hosts []string
	for _, reg := range registries {
		found := false
		for _, w := range reg.words {
			found = found || tokens[w]
		}
		for tok := range tokens {
			for _, suffix := range reg.suffixes {
				found = found || strings.HasSuffix(tok, suffix)
			}
		}
		if !found {
			continue
		}
		for _, h := range reg.hosts {
			if !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	}
	sort.Strings(hosts)
	return hosts
}

// Dockerfile is the task's environment: the verdict program's build stage, the Docker provider's
// build of env, then the base tree in the working directory as one commit, as the runner seals
// it, with the held-out files gone.
func Dockerfile(env sandbox.EnvSpec, heldOut []string) (string, error) {
	build, err := docker.Dockerfile(env)
	if err != nil {
		return "", err
	}
	dir := strings.TrimSuffix(env.Dir(), "/")
	uid := strconv.Itoa(sandbox.User)
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\n") }
	jsonArgs := func(args ...string) string {
		data, _ := json.Marshal(args)
		return string(data)
	}
	run := func(script string) { line("RUN " + jsonArgs("/bin/sh", "-c", script)) }

	line("FROM " + egressproxy.Builder + " AS casebox-verdict")
	line("WORKDIR /src")
	line("COPY verdict/ ./")
	line("RUN CGO_ENABLED=0 go build -trimpath -o /casebox-verdict .")
	line("")
	b.WriteString(build)
	line("USER root")
	line("COPY --from=casebox-verdict " + jsonArgs("/casebox-verdict", verdictBinary))
	rm := []string{".git", ".casebox"}
	rm = append(rm, heldOut...)
	quoted := make([]string, len(rm))
	for i, p := range rm {
		quoted[i] = shellQuote(p)
	}
	run("rm -rf -- " + strings.Join(quoted, " "))
	line("COPY " + jsonArgs("base/", dir+"/"))
	line("COPY " + jsonArgs("base.files", "/tmp/casebox-base.files"))
	run(sealScript(dir, uid))
	line("USER " + uid)
	return b.String(), nil
}

// gitEnv makes git ignore the image's system and global config and trust the working directory
// whoever runs it, as the runner's git commands do.
const gitEnv = `export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 ` +
	`GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*'`

const gitSafe = "git -c core.hooksPath=/dev/null -c core.fsmonitor=false"

// sealScript commits the base files as the only commit of a fresh repository, by casebox at
// 2000-01-01, on refs/heads/main with no reflog, ignores what the install steps left in the
// working directory, and gives the working directory to the sandbox user.
func sealScript(dir, uid string) string {
	return strings.Join([]string{
		"set -e",
		"command -v git >/dev/null 2>&1 || { echo 'the environment has no git: the recipe image or install steps must provide it, because the base tree is committed in the image' >&2; exit 1; }",
		gitEnv,
		"export GIT_AUTHOR_NAME=casebox GIT_AUTHOR_EMAIL=casebox@localhost GIT_AUTHOR_DATE='946684800 +0000' " +
			"GIT_COMMITTER_NAME=casebox GIT_COMMITTER_EMAIL=casebox@localhost GIT_COMMITTER_DATE='946684800 +0000'",
		"cd " + shellQuote(dir),
		"git init -q",
		"git config user.name casebox",
		"git config user.email casebox@localhost",
		"git config commit.gpgsign false",
		gitSafe + " update-index --add -z --stdin </tmp/casebox-base.files",
		`commit=$(git commit-tree -m base "$(git write-tree)")`,
		"git -c core.logAllRefUpdates=false symbolic-ref HEAD refs/heads/main",
		`git -c core.logAllRefUpdates=false update-ref refs/heads/main "$commit"`,
		"rm -rf .git/logs /tmp/casebox-base.files",
		"mkdir -p .git/info",
		gitSafe + ` -c core.quotePath=false ls-files --others --exclude-standard --directory | sed -e 's/[][*?\\]/\\&/g' -e 's|^|/|' >>.git/info/exclude`,
		`test -z "$(` + gitSafe + ` status --porcelain --untracked-files=all)" || { echo 'the base repository is not clean after ignoring what the environment left' >&2; exit 1; }`,
		"chown -R " + uid + ":" + uid + " " + shellQuote(dir) + " " + docker.ResultsDir,
	}, "\n")
}

// composeFile adds the recipe's services beside Harbor's main container, or nil without any.
func composeFile(services []sandbox.Service) ([]byte, error) {
	if len(services) == 0 {
		return nil, nil
	}
	type service struct {
		Image       string            `yaml:"image,omitempty"`
		Environment map[string]string `yaml:"environment,omitempty"`
		DependsOn   []string          `yaml:"depends_on,omitempty"`
	}
	all := map[string]service{}
	var names []string
	for _, s := range services {
		if s.Name == "main" {
			return nil, errors.New(`a recipe service is named "main", the name Harbor keeps for the task's container`)
		}
		all[s.Name] = service{Image: s.Image, Environment: s.Env}
		names = append(names, s.Name)
	}
	sort.Strings(names)
	all["main"] = service{DependsOn: names}
	return yaml.Marshal(map[string]any{"services": all})
}

// TestScript is tests/test.sh. It needs only a POSIX shell, git and the verdict program the
// environment built. The CASEBOX_* variables move its directories, for tests of the script.
func TestScript(workdir string, heldOut []string, atBase bool, commands []repo.TestCommand) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\n") }
	line("#!/bin/sh")
	line("# The Casebox verifier: restore the held-out test files as they are at the base, apply the")
	line("# held-out tests, run the recipe's test commands, and let the verdict program decide from")
	line("# their result files. The reward is 1 when every fail-to-pass and pass-to-pass test passes.")
	line("set -u")
	line("tests=${CASEBOX_TESTS_DIR:-/tests}")
	line("logs=${CASEBOX_LOGS_DIR:-/logs/verifier}")
	line("results=${CASEBOX_RESULTS_DIR:-" + docker.ResultsDir + "}")
	line("workdir=${CASEBOX_WORKDIR:-" + shellQuote(workdir) + "}")
	line("verdict=${CASEBOX_VERDICT:-" + verdictBinary + "}")
	line(gitEnv)
	line(`mkdir -p "$logs"`)
	line(`fail() {`)
	line(`  echo "$1" >&2`)
	line(`  "$verdict" -oracle "$tests/oracle.json" -out "$logs" -error "$1" || echo 0 >"$logs/reward.txt"`)
	line(`  exit 0`)
	line(`}`)
	line(`cd "$workdir" || fail "the working directory $workdir is missing"`)
	line("")
	line("# The agent's changes to the held-out files never count.")
	for _, f := range heldOut {
		line("rm -rf -- " + shellQuote(f))
	}
	if atBase {
		line(`cp -R "$tests/base/." "$workdir/" || fail "the held-out files could not be restored"`)
	}
	line(`if [ -s "$tests/tests.patch" ]; then`)
	line(`  ` + gitSafe + ` apply --whitespace=nowarn "$tests/tests.patch" 2>"$logs/apply.log" || fail "the held-out tests do not apply: $(cat "$logs/apply.log")"`)
	line(`fi`)
	line("")
	line(`set --`)
	for i, c := range commands {
		n := strconv.Itoa(i)
		line("# " + strings.ReplaceAll(c.Command, "\n", " "))
		line(`find "$results" -mindepth 1 -delete 2>/dev/null || mkdir -p "$results"`)
		cmd := "sh -c " + shellQuote(c.Command)
		secs := strconv.Itoa(int(math.Ceil(c.OrDefault().Seconds())))
		line(`if command -v timeout >/dev/null 2>&1; then timeout ` + secs + ` ` + cmd + `; else ` + cmd + `; fi >"$logs/command-` + n + `.log" 2>&1`)
		line(`echo "exit $?" >>"$logs/command-` + n + `.log"`)
		if c.Results != "" {
			line(`mkdir -p "$logs/results/` + n + `"`)
			line(`cp -R "$results/." "$logs/results/` + n + `/" 2>/dev/null`)
			line(`set -- "$@" ` + shellQuote(c.Results) + `="$logs/results/` + n + `"`)
		}
	}
	line("")
	line(`"$verdict" -oracle "$tests/oracle.json" -out "$logs" "$@" || echo 0 >"$logs/reward.txt"`)
	return b.String()
}

func solveScript(workdir string) string {
	return "#!/bin/sh\n" +
		"# Applies the merged source change: the reference solution.\n" +
		"set -eu\n" +
		gitEnv + "\n" +
		"cd " + shellQuote(workdir) + "\n" +
		gitSafe + " apply --whitespace=nowarn /solution/solution.patch\n"
}

func nulList(files []string) []byte {
	var b bytes.Buffer
	for _, f := range files {
		b.WriteString(f)
		b.WriteByte(0)
	}
	return b.Bytes()
}

// cleanPath is a slash-separated path inside the repository, without "./".
func cleanPath(name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if name == "" || clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "\x00") {
		return "", fmt.Errorf("%q is not a path inside the repository", name)
	}
	return clean, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
