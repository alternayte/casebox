package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Target is one agent's hook configuration file.
type Target struct {
	Agent  string
	Path   string
	Events []string
	// Codex runs a new hook only after the person trusts it in Codex's /hooks screen.
	NeedsTrust bool
}

// Targets lists where each agent reads user-level hooks.
func Targets(home string) []Target {
	return []Target{
		{Agent: ClaudeCode, Path: filepath.Join(home, ".claude", "settings.json"), Events: []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "SessionEnd"}},
		{Agent: Codex, Path: filepath.Join(home, ".codex", "hooks.json"), Events: []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "SessionEnd"}, NeedsTrust: true},
		{Agent: Cursor, Path: filepath.Join(home, ".cursor", "hooks.json"), Events: []string{"sessionStart", "beforeSubmitPrompt", "afterAgentResponse", "postToolUse", "afterFileEdit", "stop", "sessionEnd"}},
	}
}

// ownHook recognizes a handler Casebox wrote: its command runs a casebox binary with "hook <agent>".
var ownHook = regexp.MustCompile(`casebox(\.exe)?"?\s+hook\s+(claude-code|codex|cursor-cli)\b`)

func isOwn(h map[string]any) bool {
	cmd, _ := h["command"].(string)
	if args, ok := h["args"].([]any); ok {
		parts := []string{cmd}
		for _, a := range args {
			s, _ := a.(string)
			parts = append(parts, s)
		}
		cmd = strings.Join(parts, " ")
	}
	return ownHook.MatchString(cmd)
}

// Install writes Casebox's hooks for one agent, replacing any it wrote before and keeping every
// other hook and setting in the file.
func Install(t Target, exe string) error {
	doc, err := load(t.Path)
	if err != nil {
		return err
	}
	hooks := newObject()
	if _, err := doc.get("hooks", hooks); err != nil {
		return fmt.Errorf("%s: the hooks entry is not an object: %w", t.Path, err)
	}
	removeOwn(t, hooks)
	for _, event := range t.Events {
		var entries []any
		if _, err := hooks.get(event, &entries); err != nil {
			return fmt.Errorf("%s: hooks.%s is not a list: %w", t.Path, event, err)
		}
		entries = append(entries, entry(t, exe, event))
		if err := hooks.set(event, entries); err != nil {
			return err
		}
	}
	if err := doc.set("hooks", hooks); err != nil {
		return err
	}
	if t.Agent == Cursor {
		if _, ok := doc.values["version"]; !ok {
			if err := doc.set("version", 1); err != nil {
				return err
			}
		}
	}
	return save(t.Path, doc)
}

// Uninstall removes Casebox's hooks for one agent and keeps everything else.
func Uninstall(t Target) error {
	doc, err := load(t.Path)
	if err != nil {
		return err
	}
	hooks := newObject()
	if ok, err := doc.get("hooks", hooks); err != nil || !ok {
		return err
	}
	removeOwn(t, hooks)
	if err := doc.set("hooks", hooks); err != nil {
		return err
	}
	return save(t.Path, doc)
}

// Installed reports whether every event has Casebox's hook, and whether the hook still points
// at an existing binary.
func Installed(t Target) (installed bool, binaryOK bool, err error) {
	doc, err := load(t.Path)
	if err != nil {
		return false, false, err
	}
	hooks := newObject()
	if ok, err := doc.get("hooks", hooks); err != nil || !ok {
		return false, false, err
	}
	installed, binaryOK = true, true
	for _, event := range t.Events {
		var entries []map[string]any
		_, _ = hooks.get(event, &entries)
		found := false
		for _, e := range entries {
			for _, h := range handlers(t, e) {
				if isOwn(h) {
					found = true
					if bin := binaryOf(h); bin != "" {
						if _, err := os.Stat(bin); err != nil {
							binaryOK = false
						}
					}
				}
			}
		}
		installed = installed && found
	}
	return installed, binaryOK, nil
}

// Claude Code and Codex nest handlers in matcher groups; Cursor lists handlers directly.
func handlers(t Target, entry map[string]any) []map[string]any {
	if t.Agent == Cursor {
		return []map[string]any{entry}
	}
	list, _ := entry["hooks"].([]any)
	var out []map[string]any
	for _, h := range list {
		if m, ok := h.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func entry(t Target, exe, event string) any {
	switch t.Agent {
	case ClaudeCode:
		// Exec form: no shell, so a path with spaces needs no quoting.
		return map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": exe, "args": []string{"hook", t.Agent, event}, "timeout": 10,
		}}}
	case Codex:
		timeout := 10
		if event == "SessionEnd" {
			timeout = 3 // Codex caps SessionEnd hooks at 3 seconds
		}
		return map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": fmt.Sprintf("%q hook %s %s", exe, t.Agent, event), "timeout": timeout,
		}}}
	default:
		return map[string]any{"command": fmt.Sprintf("%q hook %s %s", exe, t.Agent, event), "timeout": 10}
	}
}

// removeOwn drops Casebox's handlers from every event, and any group or event left empty.
func removeOwn(t Target, hooks *object) {
	for _, event := range append([]string(nil), hooks.keys...) {
		var entries []map[string]any
		if _, err := hooks.get(event, &entries); err != nil {
			continue
		}
		var kept []map[string]any
		for _, e := range entries {
			if t.Agent == Cursor {
				if !isOwn(e) {
					kept = append(kept, e)
				}
				continue
			}
			var inner []any
			for _, h := range handlers(t, e) {
				if !isOwn(h) {
					inner = append(inner, h)
				}
			}
			if len(inner) > 0 {
				e["hooks"] = inner
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			hooks.remove(event)
		} else {
			_ = hooks.set(event, kept)
		}
	}
}

func binaryOf(h map[string]any) string {
	cmd, _ := h["command"].(string)
	if _, hasArgs := h["args"]; hasArgs {
		return cmd
	}
	if strings.HasPrefix(cmd, `"`) {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			return cmd[1 : end+1]
		}
	}
	return strings.Fields(cmd)[0]
}

func load(path string) (*object, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newObject(), nil
	}
	if err != nil {
		return nil, err
	}
	doc := newObject()
	if len(strings.TrimSpace(string(data))) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON; fix it by hand first: %w", path, err)
	}
	return doc, nil
}

func save(path string, doc *object) error {
	data, err := indent(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		// Keep one backup of the person's file before the first change.
		backup := path + ".casebox-backup"
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			if orig, err := os.ReadFile(path); err == nil {
				_ = os.WriteFile(backup, orig, mode)
			}
		}
	}
	tmp := path + ".casebox-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
