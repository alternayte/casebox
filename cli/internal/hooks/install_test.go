package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeSettings = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "afplay done.aiff"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]
  },
  "env": {"FOO": "1"}
}
`

func TestInstallKeepsEverythingElseAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	target := Targets(dir)[0]
	if err := os.MkdirAll(filepath.Dir(target.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Path, []byte(claudeSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	exe := "/opt/tools/casebox"
	for range 2 {
		if err := Install(target, exe); err != nil {
			t.Fatal(err)
		}
	}

	data, _ := os.ReadFile(target.Path)
	text := string(data)
	for _, keep := range []string{`"afplay done.aiff"`, `"guard.sh"`, `"Bash(ls:*)"`, `"FOO": "1"`} {
		if !strings.Contains(text, keep) {
			t.Errorf("the install lost %s", keep)
		}
	}
	if strings.Index(text, `"model"`) > strings.Index(text, `"permissions"`) || strings.Index(text, `"hooks"`) > strings.Index(text, `"env"`) {
		t.Errorf("the install reordered the top-level keys:\n%s", text)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	own := 0
	for _, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if isOwn(h) {
					own++
				}
			}
		}
	}
	if own != len(target.Events) {
		t.Fatalf("found %d Casebox hooks after two installs, want one per event (%d)", own, len(target.Events))
	}
	if installed, binaryOK, err := Installed(target); err != nil || !installed || binaryOK {
		t.Fatalf("Installed = %v, %v, %v; want installed, and the missing binary reported", installed, binaryOK, err)
	}

	if err := Uninstall(target); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(target.Path)
	var want, got any
	_ = json.Unmarshal([]byte(claudeSettings), &want)
	_ = json.Unmarshal(after, &got)
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("uninstall did not restore the settings:\nwant %s\n got %s", wantJSON, gotJSON)
	}
}

func TestCursorHooksAreAFlatListWithAVersion(t *testing.T) {
	target := Targets(t.TempDir())[2]
	if err := Install(target, `C:\Program Files\casebox\casebox.exe`); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target.Path)
	var doc struct {
		Version int                         `json:"version"`
		Hooks   map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || len(doc.Hooks["sessionEnd"]) != 1 || !isOwn(doc.Hooks["sessionEnd"][0]) {
		t.Fatalf("unexpected Cursor hooks file:\n%s", data)
	}
}
