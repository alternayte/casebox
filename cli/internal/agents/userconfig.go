package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// UserConfig places a shared harness's files where the agent reads user-level configuration,
// relative to the sandbox user's home (docs/specs/harness-ci.md, "Shared harness"). Files is keyed
// by path in the shared repository. A file this agent has no place for is left out only when
// another agent reads it; any other file is an error that names it.
func (a Adapter) UserConfig(files map[string][]byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		dest, other, err := a.userPath(name, files)
		if err != nil {
			return nil, err
		}
		switch {
		case dest == ".mcp.json":
			converted, target, err := a.userMCP(body)
			if err != nil {
				return nil, fmt.Errorf("the shared harness's .mcp.json: %w", err)
			}
			out[target] = converted
		case dest != "":
			out[dest] = body
		case !other:
			return nil, fmt.Errorf("the shared harness file %s has no user-level place in %s; move it into the repositories, or remove it from the harness globs", name, a.spec.Agent)
		}
	}
	return out, nil
}

// userPath is where name goes for this agent ("" for nowhere), and whether another agent reads it.
func (a Adapter) userPath(name string, files map[string][]byte) (string, bool, error) {
	if path.IsAbs(name) || strings.Contains(name, "..") {
		return "", false, fmt.Errorf("the shared harness file %q is not a plain relative path", name)
	}
	under := func(prefix string) bool { return strings.HasPrefix(name, prefix) }
	switch a.spec.Agent {
	case ClaudeCode:
		switch {
		case name == "CLAUDE.md":
			return ".claude/CLAUDE.md", false, nil
		case name == "AGENTS.md":
			if _, ok := files["CLAUDE.md"]; ok {
				return "", true, nil
			}
			return ".claude/CLAUDE.md", false, nil
		case under(".claude/skills/"), under(".claude/commands/"), under(".claude/agents/"):
			return name, false, nil
		case name == ".mcp.json":
			return ".mcp.json", false, nil
		case under(".agents/skills/"):
			return "", true, nil
		}
	case Codex:
		switch {
		case name == "AGENTS.md":
			return ".codex/AGENTS.md", false, nil
		case under(".agents/skills/"):
			return name, false, nil
		case name == ".mcp.json":
			return ".mcp.json", false, nil
		case name == "CLAUDE.md", under(".claude/skills/"), under(".claude/commands/"), under(".claude/agents/"):
			return "", true, nil
		}
	default:
		return "", false, fmt.Errorf("%s has no user-level configuration for a shared harness", a.spec.Agent)
	}
	return "", false, nil
}

// userMCP turns .mcp.json's servers into the agent's user-level MCP configuration.
func (a Adapter) userMCP(body []byte) ([]byte, string, error) {
	var doc struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "", err
	}
	if a.spec.Agent == ClaudeCode {
		out, err := json.MarshalIndent(map[string]any{"mcpServers": doc.Servers}, "", "  ")
		return out, ".claude.json", err
	}
	var b bytes.Buffer
	names := make([]string, 0, len(doc.Servers))
	for name := range doc.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var s struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			URL     string            `json:"url"`
		}
		if err := json.Unmarshal(doc.Servers[name], &s); err != nil {
			return nil, "", fmt.Errorf("server %s: %w", name, err)
		}
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", tomlString(name))
		if s.URL != "" {
			fmt.Fprintf(&b, "url = %s\n", tomlString(s.URL))
		}
		if s.Command != "" {
			fmt.Fprintf(&b, "command = %s\n", tomlString(s.Command))
		}
		if len(s.Args) > 0 {
			quoted := make([]string, len(s.Args))
			for i, arg := range s.Args {
				quoted[i] = tomlString(arg)
			}
			fmt.Fprintf(&b, "args = [%s]\n", strings.Join(quoted, ", "))
		}
		if len(s.Env) > 0 {
			keys := make([]string, 0, len(s.Env))
			for k := range s.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pairs := make([]string, len(keys))
			for i, k := range keys {
				pairs[i] = tomlString(k) + " = " + tomlString(s.Env[k])
			}
			fmt.Fprintf(&b, "env = { %s }\n", strings.Join(pairs, ", "))
		}
		b.WriteString("\n")
	}
	return b.Bytes(), ".codex/config.toml", nil
}

// tomlString is a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
