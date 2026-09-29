// Package recipe drafts, checks and prepares a workspace's environment recipe
// (docs/specs/sandboxes.md). Automated setup fails often, so the recipe is drafted from what the
// repository already has, checked by building it and running the tests, and confirmed by a person.
package recipe

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/alternayte/casebox/cli/internal/repo"
)

// Note says where a drafted field came from, or what a person should look at.
type Note struct {
	Field string
	Text  string
}

// Draft is a drafted recipe with its notes.
type Draft struct {
	Recipe repo.Recipe
	Notes  []Note
}

// language is what the draft knows about one ecosystem found in the repository.
type language struct {
	name      string
	dir       string // repository-relative directory of its manifest, "" for the root
	image     string
	install   []string
	lockfiles []string
	test      repo.TestCommand
	files     int // how much of the repository it covers, to pick the main one
}

// knownServices are compose or CI service images the draft carries over, by name prefix.
var knownServices = []string{"postgres", "mysql", "mariadb", "redis", "rabbitmq", "mongo"}

// FromRepository drafts the recipe of the repository at root from its tracked files: the
// devcontainer, a root Dockerfile, compose files and CI workflows first, then the languages it
// finds.
func FromRepository(root string, files []string) Draft {
	d := Draft{}
	has := map[string]bool{}
	for _, f := range files {
		has[f] = true
	}
	read := func(name string) []byte {
		data, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		return data
	}

	langs := languages(files, read)
	var main *language
	for i := range langs {
		if main == nil || langs[i].files > main.files {
			main = &langs[i]
		}
	}
	if main != nil {
		d.Recipe.Image = main.image
		d.Notes = append(d.Notes, Note{"image", fmt.Sprintf("the official %s image, for %s", main.name, where(main.dir))})
	}
	for _, l := range langs {
		d.Recipe.Install = append(d.Recipe.Install, inDir(l.dir, l.install)...)
		d.Recipe.Lockfiles = append(d.Recipe.Lockfiles, l.lockfiles...)
		t := l.test
		t.Command = inDir(l.dir, []string{t.Command})[0]
		d.Recipe.Test = append(d.Recipe.Test, t)
		if main != nil && l.name != main.name {
			d.Notes = append(d.Notes, Note{"image", fmt.Sprintf("%s also needs %s tools; add them to the image or the install steps", where(l.dir), l.name)})
		}
	}

	if image, source := devcontainerImage(has, read); image != "" {
		d.Recipe.Image = image
		d.Notes = append(d.Notes, Note{"image", "from " + source})
	} else if has["Dockerfile"] {
		if from := firstFrom(read("Dockerfile")); from != "" {
			d.Recipe.Image = from
			d.Notes = append(d.Notes, Note{"image", "the first stage of the root Dockerfile"})
		}
	}

	d.Recipe.Services = map[string]repo.Service{}
	for _, f := range files {
		base := path.Base(f)
		if path.Dir(f) == "." && (base == "compose.yaml" || base == "compose.yml" || base == "docker-compose.yaml" || base == "docker-compose.yml") {
			for name, s := range composeServices(read(f)) {
				d.Recipe.Services[name] = s
				d.Notes = append(d.Notes, Note{"services", fmt.Sprintf("%s from %s", name, f)})
			}
		}
	}
	for _, f := range files {
		if !strings.HasPrefix(f, ".github/workflows/") || !(strings.HasSuffix(f, ".yml") || strings.HasSuffix(f, ".yaml")) {
			continue
		}
		services, tests := workflow(read(f))
		for name, s := range services {
			if _, ok := d.Recipe.Services[name]; !ok {
				d.Recipe.Services[name] = s
				d.Notes = append(d.Notes, Note{"services", fmt.Sprintf("%s from %s", name, f)})
			}
		}
		for _, t := range tests {
			d.Notes = append(d.Notes, Note{"test", fmt.Sprintf("%s runs %q; compare it with the drafted test commands", f, t)})
		}
	}
	if len(d.Recipe.Services) == 0 {
		d.Recipe.Services = nil
	}
	if d.Recipe.Image == "" {
		d.Notes = append(d.Notes, Note{"image", "no language or container file was found; name the image and the test command by hand"})
	}
	return d
}

func where(dir string) string {
	if dir == "" {
		return "the repository root"
	}
	return dir + "/"
}

func inDir(dir string, commands []string) []string {
	if dir == "" {
		return commands
	}
	out := make([]string, len(commands))
	for i, c := range commands {
		out[i] = "cd " + dir + " && " + c
	}
	return out
}

func prefixed(dir string, globs ...string) []string {
	out := make([]string, len(globs))
	for i, g := range globs {
		if dir == "" {
			out[i] = g
		} else {
			out[i] = dir + "/" + g
		}
	}
	return out
}

var goVersion = regexp.MustCompile(`(?m)^go (\d+\.\d+)`)
var targetFramework = regexp.MustCompile(`<TargetFrameworks?>net(\d+)\.\d`)

// languages finds each ecosystem by its manifest, at most three directories deep.
func languages(files []string, read func(string) []byte) []language {
	count := func(dir, ext string) int {
		n := 0
		for _, f := range files {
			if (dir == "" || strings.HasPrefix(f, dir+"/")) && strings.HasSuffix(f, ext) {
				n++
			}
		}
		return n
	}
	var out []language
	seen := map[string]bool{}
	add := func(l language) {
		if !seen[l.name+"|"+l.dir] {
			seen[l.name+"|"+l.dir] = true
			out = append(out, l)
		}
	}
	for _, f := range files {
		if strings.Count(f, "/") > 3 {
			continue
		}
		dir, base := path.Dir(f), path.Base(f)
		if dir == "." {
			dir = ""
		}
		switch {
		case base == "go.mod":
			version := "1"
			if m := goVersion.FindSubmatch(read(f)); m != nil {
				version = string(m[1])
			}
			add(language{name: "Go", dir: dir, image: "golang:" + version, install: []string{"go mod download"},
				lockfiles: prefixed(dir, "go.mod", "go.sum"),
				test:      repo.TestCommand{Command: "go test -json ./... > /results/go-test.json", Results: "go-test-json"},
				files:     count(dir, ".go")})
		case strings.HasSuffix(base, ".sln") || strings.HasSuffix(base, ".slnx") || (strings.HasSuffix(base, ".csproj") && !seen[".NET|"+dotnetRoot(files, dir)]):
			root := dotnetRoot(files, dir)
			version := "10.0"
			for _, p := range files {
				if strings.HasSuffix(p, ".csproj") && (root == "" || strings.HasPrefix(p, root+"/")) {
					if m := targetFramework.FindSubmatch(read(p)); m != nil {
						version = string(m[1]) + ".0"
						break
					}
				}
			}
			add(language{name: ".NET", dir: root, image: "mcr.microsoft.com/dotnet/sdk:" + version, install: []string{"dotnet restore"},
				lockfiles: append(prefixed(root, "**/*.csproj", "**/packages.lock.json", "**/Directory.Packages.props", "**/Directory.Build.props", "global.json", "*.sln", "*.slnx"), "global.json"),
				test:      repo.TestCommand{Command: "dotnet test --logger trx --results-directory /results", Results: "trx"},
				files:     count(root, ".cs")})
		case base == "package.json" && !strings.Contains(f, "node_modules/"):
			l := language{name: "JavaScript", dir: dir, lockfiles: prefixed(dir, "package.json"),
				test: repo.TestCommand{Command: "npm test", Results: "junit"}, files: count(dir, ".ts") + count(dir, ".tsx") + count(dir, ".js")}
			lock := func(name string) bool { return contains(files, strings.TrimPrefix(dir+"/"+name, "/")) }
			switch {
			case lock("bun.lock") || lock("bun.lockb"):
				l.image, l.install = "oven/bun:1", []string{"bun install --frozen-lockfile"}
				l.lockfiles = append(l.lockfiles, prefixed(dir, "bun.lock", "bun.lockb")...)
				l.test.Command = "bun test --reporter=junit --reporter-outfile=/results/junit.xml"
			case lock("pnpm-lock.yaml"):
				l.image, l.install = "node:22", []string{"corepack enable && pnpm install --frozen-lockfile"}
				l.lockfiles = append(l.lockfiles, prefixed(dir, "pnpm-lock.yaml")...)
				l.test.Command = "pnpm test"
			case lock("yarn.lock"):
				l.image, l.install = "node:22", []string{"corepack enable && yarn install --immutable"}
				l.lockfiles = append(l.lockfiles, prefixed(dir, "yarn.lock")...)
				l.test.Command = "yarn test"
			default:
				l.image, l.install = "node:22", []string{"npm ci"}
				l.lockfiles = append(l.lockfiles, prefixed(dir, "package-lock.json")...)
			}
			add(l)
		case base == "pyproject.toml" || base == "requirements.txt":
			install := "pip install -e ."
			if base == "requirements.txt" {
				install = "pip install -r requirements.txt"
			}
			add(language{name: "Python", dir: dir, image: "python:3.12", install: []string{install},
				lockfiles: prefixed(dir, "pyproject.toml", "requirements*.txt", "poetry.lock", "uv.lock"),
				test:      repo.TestCommand{Command: "python -m pytest --junitxml=/results/junit.xml", Results: "junit"},
				files:     count(dir, ".py")})
		case base == "pom.xml":
			add(language{name: "Java", dir: dir, image: "maven:3-eclipse-temurin-21", install: []string{"mvn -q -DskipTests dependency:go-offline"},
				lockfiles: prefixed(dir, "**/pom.xml"),
				test:      repo.TestCommand{Command: "mvn -q test && cp -r target/surefire-reports /results/", Results: "junit"},
				files:     count(dir, ".java")})
		case base == "build.gradle" || base == "build.gradle.kts":
			add(language{name: "Java", dir: dir, image: "gradle:8-jdk21", install: []string{"gradle --no-daemon dependencies"},
				lockfiles: prefixed(dir, "**/*.gradle", "**/*.gradle.kts", "gradle/libs.versions.toml"),
				test:      repo.TestCommand{Command: "gradle --no-daemon test && cp -r build/test-results /results/", Results: "junit"},
				files:     count(dir, ".java") + count(dir, ".kt")})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}

// dotnetRoot is the directory of the solution above a project, or the project's own.
func dotnetRoot(files []string, dir string) string {
	best := dir
	for _, f := range files {
		if strings.HasSuffix(f, ".sln") || strings.HasSuffix(f, ".slnx") {
			d := path.Dir(f)
			if d == "." {
				d = ""
			}
			if d == "" || dir == d || strings.HasPrefix(dir, d+"/") {
				if len(d) < len(best) || best == dir {
					best = d
				}
			}
		}
	}
	return best
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var jsonComments = regexp.MustCompile(`(?m)^\s*//.*$|/\*[\s\S]*?\*/`)

func devcontainerImage(has map[string]bool, read func(string) []byte) (string, string) {
	for _, name := range []string{".devcontainer/devcontainer.json", ".devcontainer.json"} {
		if !has[name] {
			continue
		}
		var dc struct {
			Image string `json:"image"`
			Build struct {
				Dockerfile string `json:"dockerfile"`
			} `json:"build"`
			DockerFile string `json:"dockerFile"`
		}
		if json.Unmarshal(jsonComments.ReplaceAll(read(name), nil), &dc) != nil {
			continue
		}
		if dc.Image != "" {
			return dc.Image, name
		}
		dockerfile := firstNonEmpty(dc.Build.Dockerfile, dc.DockerFile)
		if dockerfile != "" {
			p := path.Join(path.Dir(name), dockerfile)
			if from := firstFrom(read(p)); from != "" {
				return from, p + " (via " + name + ")"
			}
		}
	}
	return "", ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var fromLine = regexp.MustCompile(`(?im)^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)`)

func firstFrom(dockerfile []byte) string {
	m := fromLine.FindSubmatch(dockerfile)
	if m == nil || strings.Contains(string(m[1]), "$") {
		return ""
	}
	return string(m[1])
}

func knownService(image string) bool {
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for _, k := range knownServices {
		if strings.HasPrefix(name, k) {
			return true
		}
	}
	return false
}

func composeServices(data []byte) map[string]repo.Service {
	var c struct {
		Services map[string]struct {
			Image       string    `yaml:"image"`
			Environment yaml.Node `yaml:"environment"`
		} `yaml:"services"`
	}
	out := map[string]repo.Service{}
	if yaml.Unmarshal(data, &c) != nil {
		return out
	}
	for name, s := range c.Services {
		if s.Image != "" && knownService(s.Image) && !strings.Contains(s.Image, "$") {
			out[name] = repo.Service{Image: s.Image, Env: envOf(s.Environment)}
		}
	}
	return out
}

// envOf reads a compose environment written as a map or as a list of KEY=value, keeping only
// literal values.
func envOf(n yaml.Node) map[string]string {
	env := map[string]string{}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			env[n.Content[i].Value] = n.Content[i+1].Value
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if k, v, ok := strings.Cut(item.Value, "="); ok {
				env[k] = v
			}
		}
	}
	for k, v := range env {
		if strings.Contains(v, "$") {
			delete(env, k)
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

var testRun = regexp.MustCompile(`\b(go test|dotnet test|npm (run )?test|bun (run )?test|pnpm (run )?test|yarn (run )?test|pytest|mvn\b.*\btest|gradle\w*\b.*\btest|just (check|test)|make (check|test))\b`)

// workflow reads a GitHub Actions workflow: its job services and the run steps that test.
func workflow(data []byte) (map[string]repo.Service, []string) {
	var w struct {
		Jobs map[string]struct {
			Services map[string]struct {
				Image string    `yaml:"image"`
				Env   yaml.Node `yaml:"env"`
			} `yaml:"services"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	services := map[string]repo.Service{}
	var tests []string
	if yaml.Unmarshal(data, &w) != nil {
		return services, nil
	}
	for _, job := range w.Jobs {
		for name, s := range job.Services {
			if s.Image != "" && knownService(s.Image) && !strings.Contains(s.Image, "$") {
				services[name] = repo.Service{Image: s.Image, Env: envOf(s.Env)}
			}
		}
		for _, step := range job.Steps {
			for _, line := range strings.Split(step.Run, "\n") {
				if line = strings.TrimSpace(line); testRun.MatchString(line) {
					tests = append(tests, line)
				}
			}
		}
	}
	return services, tests
}
