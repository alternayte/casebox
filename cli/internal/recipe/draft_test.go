package recipe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/alternayte/casebox/cli/internal/repo"
)

func write(t *testing.T, root string, files map[string]string) []string {
	t.Helper()
	var names []string
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func TestDraftFromAGoServiceWithPostgresAndAWebApp(t *testing.T) {
	root := t.TempDir()
	files := write(t, root, map[string]string{
		"go.mod":                   "module example.com/app\n\ngo 1.26.1\n",
		"go.sum":                   "",
		"main.go":                  "package main\n",
		"store/store.go":           "package store\n",
		"store/store_test.go":      "package store\n",
		"web/package.json":         `{"name":"web"}`,
		"web/bun.lock":             "",
		"web/src/app.ts":           "export {}\n",
		"compose.yaml":             "services:\n  db:\n    image: postgres:16\n    environment:\n      POSTGRES_PASSWORD: test\n      SECRET: ${SECRET}\n  api:\n    build: .\n",
		".github/workflows/ci.yml": "jobs:\n  test:\n    steps:\n      - run: go test ./...\n      - run: echo done\n",
	})

	d := FromRepository(root, files)
	r := d.Recipe
	if r.Image != "golang:1.26" {
		t.Fatalf("image %q, want golang:1.26 for the language with the most files", r.Image)
	}
	want := []repo.TestCommand{
		{Command: "go test -json ./... > /results/go-test.json", Results: "go-test-json"},
		{Command: "cd web && bun test --reporter=junit --reporter-outfile=/results/junit.xml", Results: "junit"},
	}
	if len(r.Test) != 2 || r.Test[0] != want[0] || r.Test[1] != want[1] {
		t.Fatalf("tests %+v", r.Test)
	}
	if strings.Join(r.Install, "|") != "go mod download|cd web && bun install --frozen-lockfile" {
		t.Fatalf("install %q", r.Install)
	}
	db, ok := r.Services["db"]
	if !ok || db.Image != "postgres:16" || db.Env["POSTGRES_PASSWORD"] != "test" || db.Env["SECRET"] != "" || len(r.Services) != 1 {
		t.Fatalf("services %+v: want only db, with its literal environment", r.Services)
	}
	for _, g := range []string{"go.mod", "go.sum", "web/package.json", "web/bun.lock"} {
		found := false
		for _, l := range r.Lockfiles {
			found = found || l == g
		}
		if !found {
			t.Fatalf("lockfiles %v miss %s", r.Lockfiles, g)
		}
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}

	// The rendered block reads back as the same recipe, with its notes as comments.
	text, err := d.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "# image: the official Go image") || !strings.Contains(text, "web/ also needs JavaScript tools") || !strings.Contains(text, `runs "go test ./..."`) {
		t.Fatalf("the notes are missing:\n%s", text)
	}
	var back struct {
		Environment repo.Recipe `yaml:"environment"`
	}
	if err := yaml.Unmarshal([]byte(text), &back); err != nil {
		t.Fatal(err)
	}
	if string(back.Environment.JSON()) != string(r.JSON()) {
		t.Fatalf("the rendered block reads back as\n%s\nnot\n%s", back.Environment.JSON(), r.JSON())
	}
}

func TestADevcontainerImageWinsOverTheLanguageDefault(t *testing.T) {
	root := t.TempDir()
	files := write(t, root, map[string]string{
		".devcontainer/devcontainer.json": "{\n  // the team's image\n  \"build\": { \"dockerfile\": \"Dockerfile\" }\n}\n",
		".devcontainer/Dockerfile":        "FROM mcr.microsoft.com/devcontainers/go:1.26 AS dev\nRUN true\n",
		"go.mod":                          "module x\n\ngo 1.25\n",
	})
	d := FromRepository(root, files)
	if d.Recipe.Image != "mcr.microsoft.com/devcontainers/go:1.26" {
		t.Fatalf("image %q, want the devcontainer's FROM", d.Recipe.Image)
	}
}
