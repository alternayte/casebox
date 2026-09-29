package runner

import (
	"reflect"
	"testing"
)

func TestDrift(t *testing.T) {
	base := []string{
		"go.mod", "cli/cmd/casebox/main.go", "cli/internal/repo/repo.go", "docs/specs/cases.md",
		"server/Casebox.slnx", "server/Api/Api.csproj", "web/package.json", ".claude/skills/review/scripts/run.sh",
	}
	harness := map[string][]byte{
		"AGENTS.md": []byte("# Rules\n\n" +
			"Run `just check` before you finish, then `make lint`.\n" +
			"Read docs/specs/cases.md and docs/specs/gone.md first; see [the plan](PLAN.md).\n" +
			"Code lives in `cli/internal/repo` and `cli/internal/runner/`. Use and/or e.g. Node.js, `sandbox.Provider` and `net/http`.\n" +
			"Never fetch https://github.com/acme/api/blob/main/x.go or edit /etc/hosts.\n" +
			"```sh\n" +
			"cd web && bun run dev && pnpm build && pnpm install\n" +
			"dotnet test server/Casebox.slnx --logger trx\n" +
			"dotnet build server/Old/Old.csproj\n" +
			"npm run storybook -- --port 6006\n" +
			"```\n"),
		".claude/skills/review/SKILL.md": []byte("Run `scripts/run.sh`, then `scripts/missing.sh`.\n"),
	}
	drift, refs, missing := Drift(harness, base, []string{"check", "cli"}, []string{"build"}, []string{"dev", "build"})
	wantRefs := []string{
		"PLAN.md", "bun run dev", "cli/internal/repo", "cli/internal/runner",
		"docs/specs/cases.md", "docs/specs/gone.md",
		"dotnet build server/Old/Old.csproj", "dotnet test server/Casebox.slnx", "just check", "make lint", "npm run storybook", "pnpm build",
		"scripts/missing.sh", "scripts/run.sh",
	}
	wantMissing := []string{
		"PLAN.md", "cli/internal/runner", "docs/specs/gone.md", "dotnet build server/Old/Old.csproj",
		"make lint", "npm run storybook", "scripts/missing.sh",
	}
	if !reflect.DeepEqual(refs, wantRefs) {
		t.Errorf("refs\n got %q\nwant %q", refs, wantRefs)
	}
	if !reflect.DeepEqual(missing, wantMissing) {
		t.Errorf("missing\n got %q\nwant %q", missing, wantMissing)
	}
	if !drift {
		t.Error("drift is false with missing references")
	}

	if drift, _, missing := Drift(map[string][]byte{"AGENTS.md": []byte("Run `just check`. See AGENTS.md.\n")}, base, []string{"check"}, nil, nil); drift {
		t.Errorf("a harness whose references all exist drifts: %q", missing)
	}
}

func TestRecipeLists(t *testing.T) {
	justfile := []byte("set windows-shell := [\"bash\", \"-cu\"]\nversion := `git describe`\n\n# The gate.\ncheck: checks cli\n\n[private]\n@checks:\n    echo ok\n" +
		"build target='all' *args:\n    go build\nalias b := build\nexport FOO := \"x\"\nimport 'other.just'\n")
	if got, want := JustRecipes(justfile), []string{"check", "checks", "build", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("JustRecipes = %q, want %q", got, want)
	}
	makefile := []byte("GO ?= go\nOUT := bin\n.PHONY: all test\nall test: deps\n\t@echo run: now\n%.o: %.c\n$(OUT)/app: main.go\nlint::\n\tgolangci-lint run\n")
	if got, want := MakeTargets(makefile), []string{"all", "test", "lint"}; !reflect.DeepEqual(got, want) {
		t.Errorf("MakeTargets = %q, want %q", got, want)
	}
	scripts, err := PackageScripts([]byte(`{"name":"web","scripts":{"dev":"vite","build":"tsc && vite build"}}`))
	if err != nil || !reflect.DeepEqual(scripts, []string{"build", "dev"}) {
		t.Errorf("PackageScripts = %q, %v", scripts, err)
	}
}
