package propose

import (
	"encoding/json"
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

// An MCP server joins the config without touching the other entries, and never carries a secret.
func TestAddMCPServer(t *testing.T) {
	existing := "{\n  \"mcpServers\": {\n    \"github\": { \"command\": \"gh-mcp\" }\n  },\n  \"other\": true\n}\n"
	files := map[string]string{".cursor/mcp.json": existing}
	server := `{"command": "npx", "args": ["-y", "@acme/db-mcp"], "env": {"DATABASE_URL": "${DATABASE_URL}"}}`
	got, err := Apply(files, []Edit{{Op: "add_mcp_server", File: ".cursor/mcp.json", Heading: "db", New: server}}, all)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers map[string]map[string]any `json:"mcpServers"`
		Other   bool                      `json:"other"`
	}
	if err := json.Unmarshal([]byte(*got[".cursor/mcp.json"]), &doc); err != nil {
		t.Fatalf("%v\n%s", err, *got[".cursor/mcp.json"])
	}
	if doc.Servers["db"]["command"] != "npx" || doc.Servers["github"]["command"] != "gh-mcp" || !doc.Other {
		t.Fatalf("%s", *got[".cursor/mcp.json"])
	}
	if !strings.Contains(*got[".cursor/mcp.json"], `"github": { "command": "gh-mcp" }`) {
		t.Fatalf("the existing entry changed: %s", *got[".cursor/mcp.json"])
	}
	// A new config file is created with the one server.
	got, err = Apply(map[string]string{}, []Edit{{Op: "add_mcp_server", File: ".mcp.json", Heading: "db", New: server}}, all)
	if err != nil || !json.Valid([]byte(*got[".mcp.json"])) {
		t.Fatalf("new file: %v %v", err, got)
	}

	for name, e := range map[string]Edit{
		"a secret value":  {Op: "add_mcp_server", File: ".cursor/mcp.json", Heading: "db", New: `{"command": "x", "env": {"TOKEN": "sk-123"}}`},
		"a taken name":    {Op: "add_mcp_server", File: ".cursor/mcp.json", Heading: "github", New: `{"command": "x"}`},
		"no command":      {Op: "add_mcp_server", File: ".cursor/mcp.json", Heading: "db", New: `{"args": []}`},
		"another file":    {Op: "add_mcp_server", File: "mcp.json", Heading: "db", New: `{"command": "x"}`},
		"a bad rule path": {Op: "write_rule", File: ".cursor/rules/Tests.md", New: "x"},
	} {
		if _, err := Apply(files, []Edit{e}, all); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Private mode writes only files git does not track, and turns shared bullets into a rule of its own.
func TestPrivate(t *testing.T) {
	files := map[string]string{"AGENTS.md": agents}
	untracked := func(string) bool { return false }
	got, err := Private("01ABC", "Say that integration tests use the real database", []Edit{
		{Op: "add_bullet", File: "AGENTS.md", Heading: "Testing", New: "Integration tests use the real database."},
		{Op: "write_skill", File: ".claude/skills/db/SKILL.md", New: "1. Start the container."},
	}, files, untracked)
	if err != nil {
		t.Fatal(err)
	}
	rule := got[".cursor/rules/casebox-01abc.mdc"]
	if !strings.Contains(rule, "alwaysApply: true") || !strings.Contains(rule, "- Integration tests use the real database.") {
		t.Fatalf("rule: %q", rule)
	}
	if _, ok := got["AGENTS.md"]; ok || got[".claude/skills/db/SKILL.md"] != "1. Start the container.\n" {
		t.Fatalf("files: %v", got)
	}
	if _, err := Private("01ABC", "t", []Edit{{Op: "delete_bullet", File: "AGENTS.md", Old: "Use tabs."}}, files, untracked); err == nil {
		t.Error("a removal stayed private")
	}
	tracked := func(string) bool { return true }
	if _, err := Private("01ABC", "t", []Edit{{Op: "write_skill", File: ".claude/skills/db/SKILL.md", New: "x"}}, files, tracked); err == nil {
		t.Error("a tracked skill stayed private")
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
