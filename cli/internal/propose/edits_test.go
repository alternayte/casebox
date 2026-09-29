package propose

import (
	"strings"
	"testing"
)

const agents = `# Agents

## Testing

- Run the unit tests with just test.

## Style

- Use tabs.
- Keep functions short.
`

var all = func(string) bool { return true }

// Every edit changes only what it names, and the guard rails refuse the rest.
func TestApply(t *testing.T) {
	files := map[string]string{"AGENTS.md": agents, ".claude/skills/review/SKILL.md": "Review.\n"}
	got, err := Apply(files, []Edit{
		{Op: "add_bullet", File: "AGENTS.md", Heading: "Testing", New: "- Integration tests use the real database; do not mock it."},
		{Op: "replace_bullet", File: "AGENTS.md", Old: "Keep functions short.", New: "Keep functions under 40 lines."},
	}, all)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(agents, "- Run the unit tests with just test.\n", "- Run the unit tests with just test.\n- Integration tests use the real database; do not mock it.\n", 1)
	want = strings.Replace(want, "- Keep functions short.", "- Keep functions under 40 lines.", 1)
	if got["AGENTS.md"] == nil || *got["AGENTS.md"] != want || len(got) != 1 {
		t.Fatalf("got %q\nwant %q", deref(got["AGENTS.md"]), want)
	}

	// A heading that does not exist is created at the end.
	got, _ = Apply(files, []Edit{{Op: "add_bullet", File: "AGENTS.md", Heading: "Database", New: "Use migrations."}}, all)
	if !strings.HasSuffix(*got["AGENTS.md"], "## Database\n\n- Use migrations.\n") {
		t.Fatalf("new heading: %q", *got["AGENTS.md"])
	}

	got, _ = Apply(files, []Edit{{Op: "delete_section", File: "AGENTS.md", Heading: "## Style"}}, all)
	if strings.Contains(*got["AGENTS.md"], "Style") || !strings.Contains(*got["AGENTS.md"], "## Testing") {
		t.Fatalf("delete_section: %q", *got["AGENTS.md"])
	}
	got, _ = Apply(files, []Edit{{Op: "delete_skill", File: ".claude/skills/review/SKILL.md"}}, all)
	if v, ok := got[".claude/skills/review/SKILL.md"]; !ok || v != nil {
		t.Fatalf("delete_skill: %v", got)
	}
	got, _ = Apply(files, []Edit{{Op: "write_skill", File: ".claude/skills/migrate/SKILL.md", New: "1. Write the migration.\n2. Run it."}}, all)
	if *got[".claude/skills/migrate/SKILL.md"] != "1. Write the migration.\n2. Run it.\n" {
		t.Fatalf("write_skill: %q", *got[".claude/skills/migrate/SKILL.md"])
	}

	for name, edits := range map[string][]Edit{
		"four edits":           {{Op: "add_bullet", File: "AGENTS.md", Heading: "A", New: "a"}, {Op: "add_bullet", File: "AGENTS.md", Heading: "B", New: "b"}, {Op: "add_bullet", File: "AGENTS.md", Heading: "C", New: "c"}, {Op: "add_bullet", File: "AGENTS.md", Heading: "D", New: "d"}},
		"a missing bullet":     {{Op: "replace_bullet", File: "AGENTS.md", Old: "Nothing like this.", New: "x"}},
		"an unknown op":        {{Op: "rewrite", File: "AGENTS.md", New: "x"}},
		"a path out":           {{Op: "add_bullet", File: "../AGENTS.md", Heading: "A", New: "a"}},
		"a skill path":         {{Op: "write_skill", File: "docs/SKILL.md", New: "x"}},
		"a whole-file rewrite": {{Op: "delete_section", File: "AGENTS.md", Heading: "Testing"}, {Op: "delete_section", File: "AGENTS.md", Heading: "Style"}},
	} {
		if _, err := Apply(files, edits, all); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := Apply(files, []Edit{{Op: "add_bullet", File: "src/main.go", Heading: "A", New: "a"}}, func(f string) bool { return f == "AGENTS.md" }); err == nil {
		t.Error("a file outside the harness globs was accepted")
	}
}

func TestSectionsLargestFirst(t *testing.T) {
	parts := Sections(agents)
	if len(parts) != 2 || parts[0].Heading != "Testing" || parts[1].Heading != "Style" {
		t.Fatalf("%+v", parts)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
