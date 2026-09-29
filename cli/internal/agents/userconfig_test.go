package agents

import (
	"strings"
	"testing"
)

// A shared harness lands where each agent reads user-level configuration; a file only the other
// agent reads is left out; a file no agent reads refuses the run; .mcp.json becomes each agent's
// own MCP format.
func TestUserConfig(t *testing.T) {
	shared := map[string][]byte{
		"AGENTS.md":                      []byte("rules\n"),
		".claude/skills/review/SKILL.md": []byte("review\n"),
		".agents/skills/test/SKILL.md":   []byte("test\n"),
		".mcp.json":                      []byte(`{"mcpServers":{"db":{"command":"npx","args":["-y","db \"mcp\""],"env":{"URL":"x"}}}}`),
	}
	claude, _ := For(Spec{Agent: ClaudeCode, AgentVersion: "2.1.284", Model: "m-1", Harness: "main"})
	got, err := claude.UserConfig(shared)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[".claude/CLAUDE.md"]) != "rules\n" || string(got[".claude/skills/review/SKILL.md"]) != "review\n" || len(got) != 3 {
		t.Fatalf("claude code: %q", got)
	}
	if !strings.Contains(string(got[".claude.json"]), `"mcpServers"`) || !strings.Contains(string(got[".claude.json"]), `"npx"`) {
		t.Fatalf("claude code mcp: %s", got[".claude.json"])
	}

	codex, _ := For(Spec{Agent: Codex, AgentVersion: "0.159.0", Model: "m-1", Harness: "main"})
	got, err = codex.UserConfig(shared)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[".codex/AGENTS.md"]) != "rules\n" || string(got[".agents/skills/test/SKILL.md"]) != "test\n" || len(got) != 3 {
		t.Fatalf("codex: %q", got)
	}
	toml := string(got[".codex/config.toml"])
	for _, want := range []string{`[mcp_servers."db"]`, `command = "npx"`, `args = ["-y", "db \"mcp\""]`, `env = { "URL" = "x" }`} {
		if !strings.Contains(toml, want) {
			t.Fatalf("codex mcp lacks %s:\n%s", want, toml)
		}
	}

	// CLAUDE.md wins over AGENTS.md for Claude Code; Codex reads only AGENTS.md.
	both := map[string][]byte{"CLAUDE.md": []byte("claude\n"), "AGENTS.md": []byte("agents\n")}
	if got, _ := claude.UserConfig(both); string(got[".claude/CLAUDE.md"]) != "claude\n" || len(got) != 1 {
		t.Fatalf("claude code with both: %q", got)
	}

	if _, err := claude.UserConfig(map[string][]byte{".cursor/rules/x.mdc": []byte("x")}); err == nil || !strings.Contains(err.Error(), ".cursor/rules/x.mdc") {
		t.Fatalf("a file no agent reads: %v", err)
	}
	if _, err := claude.UserConfig(map[string][]byte{".claude/skills/../../x": []byte("x")}); err == nil {
		t.Fatal("a path out of the home was accepted")
	}
	if err := Validate(Spec{Agent: CursorCLI, AgentVersion: "2026.06.19", Model: "m-1", Harness: "main", Shared: &Shared{Repo: "r", Ref: "HEAD"}}); err == nil {
		t.Fatal("cursor-cli with a shared harness was accepted")
	}
}
