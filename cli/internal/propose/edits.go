// Package propose drafts proposals (docs/specs/simple-evolution.md): one small change to a
// repository's harness files per pattern, drafted by the worker's analysis model. The same edits
// are applied twice: by the worker, to show the change for review, and by `casebox apply` on the
// person's working tree.
package propose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Edit is one small change to a harness file.
type Edit struct {
	Op      string `json:"op"`
	File    string `json:"file"`
	Heading string `json:"heading,omitempty"`
	Old     string `json:"old,omitempty"`
	New     string `json:"new,omitempty"`
}

// Ops are the edits a proposal may make.
var Ops = []string{"add_bullet", "replace_bullet", "delete_bullet", "delete_section", "write_skill", "delete_skill", "write_rule", "add_mcp_server"}

// MaxEdits is the most edits one proposal makes.
const MaxEdits = 3

var (
	skillFile = regexp.MustCompile(`^\.(claude|agents)/skills/[a-z0-9][a-z0-9_-]{0,63}/SKILL\.md$`)
	ruleFile  = regexp.MustCompile(`^\.cursor/rules/[a-z0-9][a-z0-9_-]{0,63}\.mdc$`)
	mcpFile   = regexp.MustCompile(`^(\.mcp\.json|\.cursor/mcp\.json)$`)
	mcpName   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	envRef    = regexp.MustCompile(`^\$\{[A-Z][A-Z0-9_]*\}$`)
)

// whole reports whether a file is written whole by its op: a skill, a rule or an MCP config.
func whole(name string) bool {
	return skillFile.MatchString(name) || ruleFile.MatchString(name) || mcpFile.MatchString(name)
}

// Apply applies edits to the harness files (path to content) and returns the changed files: a new
// content, or nil for a removed file. It refuses more than 3 edits, a file the globs do not match,
// an edit that finds nothing to change, and a change that keeps less than half of an instruction
// file's lines: a proposal never rewrites a whole file.
func Apply(files map[string]string, edits []Edit, matches func(string) bool) (map[string]*string, error) {
	if len(edits) == 0 || len(edits) > MaxEdits {
		return nil, fmt.Errorf("a candidate makes 1 to %d edits, not %d", MaxEdits, len(edits))
	}
	work := map[string]*string{}
	get := func(name string) (string, bool) {
		if c, ok := work[name]; ok {
			if c == nil {
				return "", false
			}
			return *c, true
		}
		c, ok := files[name]
		return c, ok
	}
	set := func(name string, content *string) { work[name] = content }

	for _, e := range edits {
		name := path.Clean(strings.TrimPrefix(e.File, "./"))
		if name == "." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
			return nil, fmt.Errorf("the file %q is not a harness path", e.File)
		}
		switch e.Op {
		case "write_skill", "delete_skill":
			if !skillFile.MatchString(name) {
				return nil, fmt.Errorf("%s names %q, which is not .claude/skills/<name>/SKILL.md or .agents/skills/<name>/SKILL.md", e.Op, e.File)
			}
		case "write_rule":
			if !ruleFile.MatchString(name) {
				return nil, fmt.Errorf("write_rule names %q, which is not .cursor/rules/<name>.mdc", e.File)
			}
		case "add_mcp_server":
			if !mcpFile.MatchString(name) {
				return nil, fmt.Errorf("add_mcp_server names %q, which is not .mcp.json or .cursor/mcp.json", e.File)
			}
		}
		if !matches(name) {
			return nil, fmt.Errorf("%s is not matched by the harness globs", name)
		}
		content, exists := get(name)
		switch e.Op {
		case "add_bullet":
			if strings.TrimSpace(e.New) == "" || strings.TrimSpace(e.Heading) == "" {
				return nil, errors.New("add_bullet needs a heading and the new bullet")
			}
			next := addBullet(content, e.Heading, bulletText(e.New))
			set(name, &next)
		case "replace_bullet", "delete_bullet":
			if !exists {
				return nil, fmt.Errorf("%s: %s does not exist", e.Op, name)
			}
			replacement := ""
			if e.Op == "replace_bullet" {
				if strings.TrimSpace(e.New) == "" {
					return nil, errors.New("replace_bullet needs the new bullet")
				}
				replacement = "- " + bulletText(e.New)
			}
			next, ok := replaceBullet(content, bulletText(e.Old), replacement, e.Op == "delete_bullet")
			if !ok {
				return nil, fmt.Errorf("%s: no bullet %q in %s", e.Op, e.Old, name)
			}
			set(name, &next)
		case "delete_section":
			if !exists {
				return nil, fmt.Errorf("delete_section: %s does not exist", name)
			}
			next, ok := deleteSection(content, e.Heading)
			if !ok {
				return nil, fmt.Errorf("delete_section: no heading %q in %s", e.Heading, name)
			}
			set(name, &next)
		case "write_skill", "write_rule":
			if strings.TrimSpace(e.New) == "" {
				return nil, fmt.Errorf("%s needs the file's text", e.Op)
			}
			body := strings.TrimRight(e.New, "\n") + "\n"
			set(name, &body)
		case "add_mcp_server":
			next, err := addMCPServer(content, e.Heading, e.New)
			if err != nil {
				return nil, err
			}
			set(name, &next)
		case "delete_skill":
			if !exists {
				return nil, fmt.Errorf("delete_skill: %s does not exist", name)
			}
			set(name, nil)
		default:
			return nil, fmt.Errorf("the op %q is not one of %s", e.Op, strings.Join(Ops, ", "))
		}
	}

	out := map[string]*string{}
	for name, next := range work {
		old, existed := files[name]
		if next != nil && existed && *next == old {
			continue
		}
		if next != nil && existed && !whole(name) && kept(old, *next) < 0.5 {
			return nil, fmt.Errorf("the change keeps less than half of %s; a proposal never rewrites a whole file", name)
		}
		out[name] = next
	}
	if len(out) == 0 {
		return nil, errors.New("the edits change nothing")
	}
	return out, nil
}

// kept is the share of old's non-empty lines that new still has.
func kept(old, next string) float64 {
	have := map[string]int{}
	for _, l := range strings.Split(next, "\n") {
		have[strings.TrimSpace(l)]++
	}
	total, still := 0, 0
	for _, l := range strings.Split(old, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		total++
		if have[t] > 0 {
			have[t]--
			still++
		}
	}
	if total == 0 {
		return 1
	}
	return float64(still) / float64(total)
}

func bulletText(s string) string {
	t := strings.TrimSpace(s)
	for _, p := range []string{"- ", "* ", "+ "} {
		t = strings.TrimPrefix(t, p)
	}
	return strings.TrimSpace(strings.ReplaceAll(t, "\n", " "))
}

type heading struct {
	level int
	text  string
}

func headingOf(line string) (heading, bool) {
	t := strings.TrimSpace(line)
	level := 0
	for level < len(t) && t[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(t) || t[level] != ' ' {
		return heading{}, false
	}
	return heading{level, strings.TrimSpace(t[level:])}, true
}

func sameHeading(a, b string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "#")))
	}
	return norm(a) == norm(b)
}

// section finds a heading's lines: its index, and the index of the next heading of the same or a
// higher level (or len(lines)).
func section(lines []string, title string) (int, int, bool) {
	for i, l := range lines {
		h, ok := headingOf(l)
		if !ok || !sameHeading(h.text, title) {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if n, ok := headingOf(lines[j]); ok && n.level <= h.level {
				end = j
				break
			}
		}
		return i, end, true
	}
	return 0, 0, false
}

func addBullet(content, title, text string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if content == "" {
		lines = nil
	}
	start, end, ok := section(lines, title)
	if !ok {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "## "+strings.TrimSpace(strings.TrimLeft(title, "#")), "", "- "+text)
		return strings.Join(lines, "\n") + "\n"
	}
	at := start + 1
	for i := start + 1; i < end; i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") {
			at = i + 1
		}
	}
	if at == start+1 {
		// No bullet yet: after the heading and a blank line.
		insert := []string{"", "- " + text}
		lines = append(lines[:at], append(insert, lines[at:]...)...)
	} else {
		lines = append(lines[:at], append([]string{"- " + text}, lines[at:]...)...)
	}
	return strings.Join(lines, "\n") + "\n"
}

func replaceBullet(content, old, replacement string, remove bool) (string, bool) {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if !(strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")) || bulletText(t) != old {
			continue
		}
		if remove {
			lines = append(lines[:i], lines[i+1:]...)
		} else {
			indent := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
			lines[i] = indent + replacement
		}
		return strings.Join(lines, "\n"), true
	}
	return content, false
}

func deleteSection(content, title string) (string, bool) {
	lines := strings.Split(content, "\n")
	start, end, ok := section(lines, title)
	if !ok {
		return content, false
	}
	return strings.Join(append(lines[:start:start], lines[end:]...), "\n"), true
}

// addMCPServer adds one server under "mcpServers" of an MCP config (name in heading, its JSON in
// text). The entry must start a local command or name a URL, and it names every secret as ${VAR}:
// a proposal never holds a key. Existing entries keep their text.
func addMCPServer(content, name, text string) (string, error) {
	if !mcpName.MatchString(name) {
		return "", fmt.Errorf("add_mcp_server needs a server name of lower-case letters, digits, - and _, not %q", name)
	}
	var server map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &server); err != nil {
		return "", fmt.Errorf("the MCP server %s is not a JSON object: %w", name, err)
	}
	if server["command"] == nil && server["url"] == nil {
		return "", fmt.Errorf("the MCP server %s names neither a command nor a url", name)
	}
	for _, key := range []string{"env", "headers"} {
		var values map[string]string
		if raw := server[key]; raw != nil {
			if err := json.Unmarshal(raw, &values); err != nil {
				return "", fmt.Errorf("the MCP server %s: %s is not a map of strings", name, key)
			}
			for k, v := range values {
				if !envRef.MatchString(v) {
					return "", fmt.Errorf("the MCP server %s sets %s.%s to a value; name it as ${VARIABLE} instead", name, key, k)
				}
			}
		}
	}
	entry, _ := json.MarshalIndent(server, "    ", "  ")
	if strings.TrimSpace(content) == "" {
		return fmt.Sprintf("{\n  \"mcpServers\": {\n    %q: %s\n  }\n}\n", name, entry), nil
	}
	var doc struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return "", fmt.Errorf("the MCP config is not valid JSON: %w", err)
	}
	if _, taken := doc.Servers[name]; taken {
		return "", fmt.Errorf("the MCP config already has a server %s", name)
	}
	// Insert after the opening brace of "mcpServers", so the rest of the file keeps its text.
	if at := strings.Index(content, `"mcpServers"`); at >= 0 {
		if brace := strings.Index(content[at:], "{"); brace >= 0 {
			pos := at + brace + 1
			sep := ","
			if len(doc.Servers) == 0 {
				sep = ""
			}
			next := content[:pos] + fmt.Sprintf("\n    %q: %s%s", name, entry, sep) + content[pos:]
			if json.Valid([]byte(next)) {
				return next, nil
			}
		}
	}
	// No "mcpServers" yet: add it as the first key of the top-level object.
	open := strings.Index(content, "{")
	if open < 0 {
		return "", errors.New("the MCP config is not a JSON object")
	}
	sep := ","
	if bytes.Equal(bytes.TrimSpace([]byte(content[open+1:])), []byte("}")) {
		sep = ""
	}
	next := content[:open+1] + fmt.Sprintf("\n  \"mcpServers\": {\n    %q: %s\n  }%s", name, entry, sep) + content[open+1:]
	if !json.Valid([]byte(next)) {
		return "", errors.New("adding the server does not give valid JSON")
	}
	return next, nil
}
