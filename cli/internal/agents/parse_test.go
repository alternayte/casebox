package agents

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/repo"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func adapter(t *testing.T, agent string) Adapter {
	t.Helper()
	versions := map[string]string{ClaudeCode: "2.1.284", Codex: "0.159.0", CursorCLI: "2026.09.28-64d2043"}
	a, err := For(Spec{Agent: agent, AgentVersion: versions[agent], Model: "m-1", Harness: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Each agent's log in its real format gives the tokens, turns, tool calls and model the run
// answer reports, and a trace from which both process checks hold.
func TestParseRecordedLogs(t *testing.T) {
	cost := 0.1234
	cases := []struct {
		agent        string
		usage        capture.Usage
		turns        int
		toolCalls    int
		model        string
		testCommands []string
	}{
		{ClaudeCode, capture.Usage{InputTokens: 117, OutputTokens: 100, CacheReadTokens: 5800, CacheWriteTokens: 600, CostUSD: &cost},
			4, 3, "claude-sonnet-5-20260901", []string{"go test ./..."}},
		// Codex counts cached tokens inside its input; they come out.
		{Codex, capture.Usage{InputTokens: 800, OutputTokens: 280, CacheReadTokens: 4400, CacheWriteTokens: 100},
			4, 3, "gpt-5.5", []string{"go test ./..."}},
		{CursorCLI, capture.Usage{InputTokens: 900, OutputTokens: 120, CacheReadTokens: 3000, CacheWriteTokens: 200},
			4, 3, "GPT-5.5", []string{"npm test"}},
	}
	for _, c := range cases {
		m, err := adapter(t, c.agent).Parse(read(t, c.agent+".jsonl"), read(t, c.agent+".stdout.jsonl"))
		if err != nil {
			t.Fatalf("%s: %v", c.agent, err)
		}
		if len(m.Warnings) > 0 {
			t.Errorf("%s warnings: %v", c.agent, m.Warnings)
		}
		if !sameUsage(m.Usage, c.usage) {
			t.Errorf("%s usage = %+v (cost %v), want %+v", c.agent, m.Usage, costOf(m.Usage), c.usage)
		}
		if m.Turns != c.turns || m.ToolCalls != c.toolCalls || m.Model != c.model {
			t.Errorf("%s turns, tool calls, model = %d, %d, %q; want %d, %d, %q", c.agent, m.Turns, m.ToolCalls, m.Model, c.turns, c.toolCalls, c.model)
		}
		for i, e := range m.Events {
			if e.Seq != int64(i) {
				t.Fatalf("%s event %d has seq %d", c.agent, i, e.Seq)
			}
		}
		ran, edited := ProcessChecks(m.Events, "/workspace", c.testCommands, repo.DefaultTestGlobs)
		if !ran || !edited {
			t.Errorf("%s process checks = ran %v, edited %v; want both", c.agent, ran, edited)
		}
	}
}

func TestParseAnnotatesCommandsAndFailures(t *testing.T) {
	m, err := adapter(t, Codex).Parse(read(t, "codex.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range m.Events {
		if e.Kind == capture.KindToolCall || e.Kind == capture.KindToolResult {
			got = append(got, e.Kind+":"+e.Tool.Name+":"+e.Tool.Status+":"+e.Attrs["command"])
		}
	}
	want := []string{
		"tool_call:exec::go test ./...",
		"tool_result:exec:error:go test ./...",
		"tool_call:apply_patch::",
		"tool_result:apply_patch:ok:",
		"tool_call:shell::go test ./parse",
		"tool_result:shell:ok:go test ./parse",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("codex tool events:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Without its stream-json output the Cursor transcript still gives the trace, and says what it
// lacks.
func TestCursorWithoutStdout(t *testing.T) {
	m, err := adapter(t, CursorCLI).Parse(read(t, "cursor-cli.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.ToolCalls != 3 || m.Model != "" || Tokens(m.Usage) != 0 || len(m.Warnings) != 1 {
		t.Fatalf("metrics = %+v", m)
	}
}

// A Claude Code run cut off by the cap or the timeout has no result line: the transcript's
// usage stands, and the run says so.
func TestClaudeCodeWithoutResult(t *testing.T) {
	out := read(t, "claude-code.stdout.jsonl")
	out = out[:bytes.LastIndex(bytes.TrimSpace(out), []byte("\n"))]
	m, err := adapter(t, ClaudeCode).Parse(read(t, "claude-code.jsonl"), out)
	if err != nil {
		t.Fatal(err)
	}
	if m.Usage.CostUSD != nil || m.Turns != 4 || Tokens(m.Usage) != 817 || len(m.Warnings) != 1 {
		t.Fatalf("metrics = %+v, warnings %v", m.Usage, m.Warnings)
	}
}

func TestCommandTemplateParsesByLogFormat(t *testing.T) {
	a, err := For(Spec{Agent: CommandCLI, AgentVersion: "1.0.0", Model: "m-1", Harness: "main",
		Command: &Command{Template: "x {instruction_file}", LogGlob: "log/*.jsonl", LogFormat: Codex}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.Parse(read(t, "codex.jsonl"), []byte("not json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.ToolCalls != 3 || m.Model != "gpt-5.5" {
		t.Fatalf("metrics = %+v", m)
	}
	none, _ := For(Spec{Agent: CommandCLI, AgentVersion: "1.0.0", Model: "m-1", Harness: "main",
		Command: &Command{Template: "x {instruction_file}", LogFormat: LogFormatNone}})
	if m, err := none.Parse(nil, nil); err != nil || m.ToolCalls != 0 || none.LogGlob("/home/casebox") != "" {
		t.Fatalf("log format none: %+v, %v", m, err)
	}
}

func sameUsage(a, b capture.Usage) bool {
	return a.InputTokens == b.InputTokens && a.OutputTokens == b.OutputTokens && a.CacheReadTokens == b.CacheReadTokens &&
		a.CacheWriteTokens == b.CacheWriteTokens && costOf(a) == costOf(b)
}

func costOf(u capture.Usage) float64 {
	if u.CostUSD == nil {
		return -1
	}
	return *u.CostUSD
}
