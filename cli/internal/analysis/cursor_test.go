package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary stands in for cursor-agent when CASEBOX_FAKE_CURSOR_AGENT says how to answer.
func TestMain(m *testing.M) {
	if mode := os.Getenv("CASEBOX_FAKE_CURSOR_AGENT"); mode != "" {
		os.Exit(fakeCursorAgent(mode))
	}
	os.Exit(m.Run())
}

func fakeCursorAgent(mode string) int {
	prompt, _ := io.ReadAll(os.Stdin)
	wd, _ := os.Getwd()
	record := map[string]any{"args": os.Args[1:], "prompt": string(prompt), "dir": wd}
	data, _ := json.Marshal(record)
	_ = os.WriteFile(os.Getenv("CASEBOX_FAKE_CURSOR_RECORD"), data, 0o600)
	switch mode {
	case "fenced":
		out, _ := json.Marshal(cursorResult{Type: "result", Subtype: "success", Result: "```json\n{\"label\":\"correction\"}\n```"})
		fmt.Println(string(out))
		return 0
	case "fails":
		fmt.Fprintln(os.Stderr, "ActionRequiredError: Named models unavailable")
		return 1
	}
	return 2
}

func TestCursorAgentAsksInAnEmptyWorkspaceWithThePromptOnStdin(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	record := filepath.Join(home, "record.json")
	t.Setenv("CASEBOX_HOME", home)
	t.Setenv("CASEBOX_FAKE_CURSOR_RECORD", record)
	t.Setenv("CASEBOX_ANALYSIS_PROVIDER", "cursor-agent")
	t.Setenv("CASEBOX_ANALYSIS_MODEL", "")
	t.Setenv("CASEBOX_CURSOR_AGENT", self)
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "auto" {
		t.Fatalf("model = %q, want auto", cfg.Model)
	}
	client := New(cfg)
	req := Request{System: "Label the intervention.", User: "no, use the real database", Tool: "label",
		Schema: map[string]any{"type": "object"}}

	t.Setenv("CASEBOX_FAKE_CURSOR_AGENT", "fenced")
	reply, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(reply.JSON) != `{"label":"correction"}` || reply.Model != "cursor-agent/auto" {
		t.Fatalf("reply = %s from %s", reply.JSON, reply.Model)
	}
	var got struct {
		Args   []string `json:"args"`
		Prompt string   `json:"prompt"`
		Dir    string   `json:"dir"`
	}
	data, _ := os.ReadFile(record)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Args, " ") != "-p --output-format json --mode ask --trust" {
		t.Fatalf("args = %v", got.Args)
	}
	if !strings.Contains(got.Prompt, "Label the intervention.") || !strings.HasSuffix(got.Prompt, "no, use the real database") {
		t.Fatalf("prompt = %q", got.Prompt)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(home, "analysis-workspace"))
	if dir, _ := filepath.EvalSymlinks(got.Dir); dir != want {
		t.Fatalf("dir = %s, want %s", got.Dir, want)
	}

	t.Setenv("CASEBOX_FAKE_CURSOR_AGENT", "fails")
	client.Attempts, client.Backoff = 2, time.Millisecond
	_, err = client.Complete(context.Background(), req)
	var invalid *InvalidOutputError
	if err == nil || errors.As(err, &invalid) || !strings.Contains(err.Error(), "Named models unavailable") {
		t.Fatalf("err = %v, want the CLI's message", err)
	}
}
