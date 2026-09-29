package propose

import (
	"fmt"
	"path"
	"strings"
)

// PrivateRule is the Cursor rule that holds a proposal's instruction bullets in private mode.
func PrivateRule(id string) string { return ".cursor/rules/casebox-" + strings.ToLower(id) + ".mdc" }

// Private turns a proposal's edits into files that only this machine has (docs/specs/
// simple-evolution.md, private mode). New bullets for a shared instruction file become one Cursor
// rule of their own; a new skill, rule or MCP config is written as itself, but only while git does
// not track that path. Removing a bullet or a section, or changing a tracked file, needs --commit.
// files holds the current text of the files the edits name; tracked says whether git tracks a path.
func Private(id, title string, edits []Edit, files map[string]string, tracked func(string) bool) (map[string]string, error) {
	out := map[string]string{}
	var bullets []string
	for _, e := range edits {
		name := path.Clean(strings.TrimPrefix(e.File, "./"))
		switch e.Op {
		case "add_bullet", "replace_bullet":
			bullets = append(bullets, "- "+bulletText(e.New))
		case "write_skill", "write_rule", "add_mcp_server":
			if tracked(name) {
				return nil, fmt.Errorf("%s is tracked by git, so the change is visible to your team; apply it with --commit", name)
			}
			changed, err := Apply(files, []Edit{e}, func(string) bool { return true })
			if err != nil {
				return nil, err
			}
			out[name] = *changed[name]
		default:
			return nil, fmt.Errorf("%s changes a shared file and cannot stay private; apply it with --commit", e.Op)
		}
	}
	if len(bullets) > 0 {
		description := strings.ReplaceAll(strings.TrimSpace(title), "\n", " ")
		out[PrivateRule(id)] = fmt.Sprintf("---\ndescription: %s\nalwaysApply: true\n---\n\n%s\n", description, strings.Join(bullets, "\n"))
	}
	return out, nil
}
