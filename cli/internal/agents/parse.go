package agents

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/sources/claudecode"
	"github.com/alternayte/casebox/cli/internal/sources/codex"
	"github.com/alternayte/casebox/cli/internal/sources/cursor"
)

// Metrics is what an agent's session log and output show about a run.
//
// Usage counts four exclusive kinds: input tokens that were not read from or written to the
// cache, output tokens, cache reads and cache writes. Codex reports cached tokens inside its
// input count; they are taken out, in the totals and in the trace. CostUSD is set only when the
// agent reports its own cost (Claude Code does, in its result line).
//
// Events is the trace: the canonical events of the existing parsers, with each tool call's shell
// command in Attrs["command"] when it ran one, and each tool result carrying the command of its
// call and status "error" when the command failed. Warnings say what could not be read.
type Metrics struct {
	Usage     capture.Usage
	Turns     int
	ToolCalls int
	Model     string
	Events    []capture.Event
	Warnings  []string
}

// Tokens is what the token cap counts: input, output and cache writes. Cache reads are left out:
// they cost about a tenth of input, and a long Claude Code run reads its whole context from the
// cache on every call, so counting them would stop such runs early.
func Tokens(u capture.Usage) int64 {
	return u.InputTokens + u.OutputTokens + u.CacheWriteTokens
}

// Parse reads the agent's session log, and its stdout when the caller kept it (nil otherwise),
// with the existing parsers. A command template with log format none has no metrics.
func (a Adapter) Parse(log, stdout []byte) (Metrics, error) {
	switch a.logFormat() {
	case ClaudeCode:
		out := stdout
		if a.spec.Agent != ClaudeCode {
			out = nil // a command template's stdout is in no known format
		}
		return parseClaudeCode(log, out)
	case Codex:
		return parseCodex(log)
	case CursorCLI:
		out := stdout
		if a.spec.Agent != CursorCLI {
			out = nil
		}
		return parseCursor(log, out)
	default:
		return Metrics{}, nil
	}
}

// jsonLines calls fn with each non-blank line of b.
func jsonLines(b []byte, fn func(n int, line []byte) error) error {
	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 1; scanner.Scan(); n++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := fn(n, line); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// call is one tool call as the second pass over a log sees it, in the order the parser emits
// tool_call events.
type call struct {
	id       string
	commands []string
	failed   bool
}

// annotate puts the calls' commands on the tool_call events, in order, and on the tool_result
// events whose call ids results lists in order; a failed call's result gets status error. It
// returns a warning instead when the counts do not match the parser's events.
func annotate(events []capture.Event, calls []call, results []string) string {
	var callIdx, resultIdx []int
	for i, e := range events {
		switch e.Kind {
		case capture.KindToolCall:
			callIdx = append(callIdx, i)
		case capture.KindToolResult, capture.KindDenial:
			resultIdx = append(resultIdx, i)
		}
	}
	if len(callIdx) != len(calls) || len(resultIdx) != len(results) {
		return fmt.Sprintf("the log's %d tool calls and %d results do not match the trace's %d and %d; commands are not in the trace", len(calls), len(results), len(callIdx), len(resultIdx))
	}
	byID := map[string]call{}
	for n, c := range calls {
		if c.id != "" {
			byID[c.id] = c
		}
		if len(c.commands) > 0 {
			setAttr(&events[callIdx[n]], "command", strings.Join(c.commands, "\n"))
		}
	}
	for n, id := range results {
		e := &events[resultIdx[n]]
		c, ok := byID[id]
		if !ok || e.Kind != capture.KindToolResult {
			continue
		}
		if len(c.commands) > 0 {
			setAttr(e, "command", strings.Join(c.commands, "\n"))
		}
		if c.failed && e.Tool != nil {
			e.Tool.Status = "error"
		}
	}
	return ""
}

func setAttr(e *capture.Event, k, v string) {
	if e.Attrs == nil {
		e.Attrs = map[string]string{}
	}
	e.Attrs[k] = v
}

func sumUsage(events []capture.Event) (capture.Usage, int) {
	var u capture.Usage
	n := 0
	for _, e := range events {
		if e.Usage == nil {
			continue
		}
		n++
		u.InputTokens += e.Usage.InputTokens
		u.OutputTokens += e.Usage.OutputTokens
		u.CacheReadTokens += e.Usage.CacheReadTokens
		u.CacheWriteTokens += e.Usage.CacheWriteTokens
	}
	return u, n
}

func countKind(events []capture.Event, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// --- Claude Code ---

type claudeLine struct {
	Type        string          `json:"type"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// claudeResult is the last line of Claude Code's stream-json output.
type claudeResult struct {
	Type         string   `json:"type"`
	Subtype      string   `json:"subtype"`
	Model        string   `json:"model"`
	NumTurns     int      `json:"num_turns"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
}

func parseClaudeCode(log, stdout []byte) (Metrics, error) {
	res, err := claudecode.Parse(bytes.NewReader(log))
	if err != nil {
		return Metrics{}, err
	}
	m := Metrics{Events: res.Events, Model: res.Session.Model}

	var calls []call
	var results []string
	err = jsonLines(log, func(n int, raw []byte) error {
		var l claudeLine
		if json.Unmarshal(raw, &l) != nil || l.IsSidechain || (l.Type != "assistant" && l.Type != "user") {
			return nil
		}
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		var blocks []claudeBlock
		if json.Unmarshal(l.Message, &msg) != nil || json.Unmarshal(msg.Content, &blocks) != nil {
			return nil
		}
		for _, b := range blocks {
			switch {
			case l.Type == "assistant" && b.Type == "tool_use":
				c := call{id: b.ID}
				var in struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(b.Input, &in) == nil && in.Command != "" {
					c.commands = []string{in.Command}
				}
				calls = append(calls, c)
			case l.Type == "user" && b.Type == "tool_result":
				results = append(results, b.ToolUseID)
			}
		}
		return nil
	})
	if err != nil {
		return Metrics{}, err
	}
	// Claude Code marks a failed command's result is_error, which the parser turns into status
	// error; the calls carry no failure of their own.
	if w := annotate(m.Events, calls, results); w != "" {
		m.Warnings = append(m.Warnings, w)
	}

	m.Usage, m.Turns = sumUsage(m.Events)
	m.ToolCalls = countKind(m.Events, capture.KindToolCall)

	if len(stdout) > 0 {
		var result *claudeResult
		_ = jsonLines(stdout, func(n int, raw []byte) error {
			var r claudeResult
			if json.Unmarshal(raw, &r) != nil {
				return nil
			}
			switch {
			case r.Type == "result":
				result = &r
			case r.Type == "system" && r.Subtype == "init" && m.Model == "":
				m.Model = r.Model
			}
			return nil
		})
		if result != nil {
			if result.TotalCostUSD != nil {
				cost := *result.TotalCostUSD
				m.Usage.CostUSD = &cost
			}
			if result.NumTurns > 0 {
				m.Turns = result.NumTurns
			}
		} else {
			m.Warnings = append(m.Warnings, "the stream-json output has no result line: the run ended before Claude Code finished")
		}
	}
	return m, nil
}

// --- Codex ---

type codexLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
		Output    json.RawMessage `json:"output"`
		Action    *struct {
			Command []string `json:"command"`
		} `json:"action"`
		// exec_command_end
		Command  json.RawMessage `json:"command"`
		ExitCode *int            `json:"exit_code"`
		// item_completed
		Item *struct {
			Type     string          `json:"type"`
			Command  json.RawMessage `json:"command"`
			ExitCode *int            `json:"exit_code"`
			Status   string          `json:"status"`
		} `json:"item"`
	} `json:"payload"`
}

var (
	// The shell command inside a code-mode exec script: tools.exec_command({"cmd":"..."}).
	codexCmd = regexp.MustCompile(`"cmd"\s*:\s*("(?:[^"\\]|\\.)*")`)
	// The exit code in a tool output's text.
	codexExit = regexp.MustCompile(`(?m)(?:Process exited with code|Exit code:?)\s*(-?\d+)`)
)

func parseCodex(log []byte) (Metrics, error) {
	res, err := codex.Parse(bytes.NewReader(log))
	if err != nil {
		return Metrics{}, err
	}
	m := Metrics{Events: res.Events, Model: res.Session.Model}

	var calls []call
	var results []string
	index := map[string]int{} // call id → position in calls
	err = jsonLines(log, func(n int, raw []byte) error {
		var l codexLine
		if json.Unmarshal(raw, &l) != nil {
			return nil
		}
		p := l.Payload
		switch {
		case l.Type == "response_item" && (p.Type == "function_call" || p.Type == "custom_tool_call" || p.Type == "local_shell_call"):
			c := call{id: p.CallID, commands: codexCommands(p.Arguments, p.Input)}
			if p.Action != nil && len(p.Action.Command) > 0 {
				c.commands = append(c.commands, shellLine(p.Action.Command))
			}
			index[p.CallID] = len(calls)
			calls = append(calls, c)
		case l.Type == "response_item" && (p.Type == "function_call_output" || p.Type == "custom_tool_call_output"):
			results = append(results, p.CallID)
			if i, ok := index[p.CallID]; ok {
				if code, ok := outputExit(p.Output); ok && code != 0 {
					calls[i].failed = true
				}
			}
		case l.Type == "event_msg" && p.Type == "exec_command_end":
			// Legacy rollouts: the command of a call and its exit code.
			i, ok := index[p.CallID]
			if !ok {
				i = len(calls) - 1
			}
			if i >= 0 {
				recordExec(&calls[i], p.Command, p.ExitCode)
			}
		case l.Type == "event_msg" && p.Type == "item_completed" && p.Item != nil && p.Item.Type == "CommandExecution":
			// Paginated rollouts: a command the latest call ran, with no call id.
			if i := len(calls) - 1; i >= 0 {
				recordExec(&calls[i], p.Item.Command, p.Item.ExitCode)
				if p.Item.Status == "failed" {
					calls[i].failed = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return Metrics{}, err
	}
	if w := annotate(m.Events, calls, results); w != "" {
		m.Warnings = append(m.Warnings, w)
	}
	for i := range m.Events {
		if u := m.Events[i].Usage; u != nil {
			n := *u
			n.InputTokens = max(u.InputTokens-u.CacheReadTokens-u.CacheWriteTokens, 0)
			m.Events[i].Usage = &n
		}
	}
	m.Usage, m.Turns = sumUsage(m.Events)
	m.ToolCalls = countKind(m.Events, capture.KindToolCall)
	return m, nil
}

// codexCommands finds the shell commands of a call: the "command" (an argv or a line) or "cmd"
// of a function call's JSON arguments, or the exec_command calls inside a code-mode script.
func codexCommands(arguments, input string) []string {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &args) == nil {
		for _, k := range []string{"command", "cmd"} {
			raw, ok := args[k]
			if !ok {
				continue
			}
			var line string
			if json.Unmarshal(raw, &line) == nil && line != "" {
				return []string{line}
			}
			var argv []string
			if json.Unmarshal(raw, &argv) == nil && len(argv) > 0 {
				return []string{shellLine(argv)}
			}
		}
	}
	var out []string
	for _, m := range codexCmd.FindAllStringSubmatch(input, -1) {
		var line string
		if json.Unmarshal([]byte(m[1]), &line) == nil && line != "" {
			out = append(out, line)
		}
	}
	return out
}

// recordExec adds an executed command to a call and marks the call failed on a non-zero exit.
func recordExec(c *call, command json.RawMessage, exit *int) {
	var line string
	var argv []string
	switch {
	case json.Unmarshal(command, &line) == nil && line != "":
	case json.Unmarshal(command, &argv) == nil && len(argv) > 0:
		line = shellLine(argv)
	}
	if line != "" && !containsString(c.commands, line) {
		c.commands = append(c.commands, line)
	}
	if exit != nil && *exit != 0 {
		c.failed = true
	}
}

// shellLine is the command an argv runs: the script of "sh -c <script>", else the words joined.
func shellLine(argv []string) string {
	if len(argv) >= 3 && (argv[len(argv)-2] == "-c" || argv[len(argv)-2] == "-lc") {
		return argv[len(argv)-1]
	}
	return strings.Join(argv, " ")
}

// outputExit reads an exit code from a tool output: the metadata of a JSON output, or the
// "Process exited with code N" line of the text.
func outputExit(raw json.RawMessage) (int, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
			text = b.String()
		}
	}
	var structured struct {
		Metadata *struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(text), &structured) == nil && structured.Metadata != nil && structured.Metadata.ExitCode != nil {
		return *structured.Metadata.ExitCode, true
	}
	if m := codexExit.FindStringSubmatch(text); m != nil {
		n, err := strconv.Atoi(m[1])
		return n, err == nil
	}
	return 0, false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- Cursor CLI ---

type cursorLine struct {
	Role    string `json:"role"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// cursorStream is a line of the Cursor CLI's stream-json output.
type cursorStream struct {
	Type     string                     `json:"type"`
	Subtype  string                     `json:"subtype"`
	Model    string                     `json:"model"`
	CallID   string                     `json:"call_id"`
	ToolCall map[string]json.RawMessage `json:"tool_call"`
	Usage    *struct {
		InputTokens      int64 `json:"inputTokens"`
		OutputTokens     int64 `json:"outputTokens"`
		CacheReadTokens  int64 `json:"cacheReadTokens"`
		CacheWriteTokens int64 `json:"cacheWriteTokens"`
	} `json:"usage"`
}

// cursorTool is the value of a tool_call entry, such as {"shellToolCall": {...}}.
type cursorTool struct {
	Args struct {
		Command string `json:"command"`
	} `json:"args"`
	Result map[string]json.RawMessage `json:"result"`
}

// parseCursor reads a Cursor transcript, which holds the conversation but no model, usage or
// tool results. The stream-json output holds those: the model in its init line, the usage in its
// result line, and each tool call's result, which is added to the trace after its call.
func parseCursor(log, stdout []byte) (Metrics, error) {
	_, events, err := cursor.Parse(bytes.NewReader(log), "cursor.jsonl", time.Time{})
	if err != nil {
		return Metrics{}, err
	}
	m := Metrics{Events: events}

	var calls []call
	turns := 0
	err = jsonLines(log, func(n int, raw []byte) error {
		var l cursorLine
		if json.Unmarshal(raw, &l) != nil || l.Role != "assistant" {
			return nil
		}
		turns++
		for _, c := range l.Message.Content {
			if c.Type != "tool_use" {
				continue
			}
			var in struct {
				Command string `json:"command"`
			}
			cl := call{}
			if json.Unmarshal(c.Input, &in) == nil && in.Command != "" {
				cl.commands = []string{in.Command}
			}
			calls = append(calls, cl)
		}
		return nil
	})
	if err != nil {
		return Metrics{}, err
	}
	if w := annotate(m.Events, calls, nil); w != "" {
		m.Warnings = append(m.Warnings, w)
	}
	m.Turns = turns

	if len(stdout) > 0 {
		var started []string
		completed := map[string]cursorTool{}
		sawResult := false
		_ = jsonLines(stdout, func(n int, raw []byte) error {
			var s cursorStream
			if json.Unmarshal(raw, &s) != nil {
				return nil
			}
			switch {
			case s.Type == "system" && s.Subtype == "init":
				m.Model = s.Model
			case s.Type == "tool_call" && s.Subtype == "started":
				started = append(started, s.CallID)
			case s.Type == "tool_call" && s.Subtype == "completed":
				for _, v := range s.ToolCall {
					var t cursorTool
					if json.Unmarshal(v, &t) == nil {
						completed[s.CallID] = t
					}
				}
			case s.Type == "result":
				sawResult = true
				if u := s.Usage; u != nil {
					m.Usage = capture.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens}
				}
			}
			return nil
		})
		if !sawResult {
			m.Warnings = append(m.Warnings, "the stream-json output has no result line: the run ended before the Cursor CLI finished")
		}
		if w := cursorResults(&m, started, completed); w != "" {
			m.Warnings = append(m.Warnings, w)
		}
	} else {
		m.Warnings = append(m.Warnings, "no stream-json output: the Cursor transcript has no model, usage or tool results")
	}
	m.ToolCalls = countKind(m.Events, capture.KindToolCall)
	return m, nil
}

// cursorResults adds a tool_result event after each tool call whose result the stream holds,
// matching the stream's calls to the transcript's in order.
func cursorResults(m *Metrics, started []string, completed map[string]cursorTool) string {
	var callIdx []int
	for i, e := range m.Events {
		if e.Kind == capture.KindToolCall {
			callIdx = append(callIdx, i)
		}
	}
	if len(callIdx) != len(started) {
		return fmt.Sprintf("the stream's %d tool calls do not match the transcript's %d; tool results are not in the trace", len(started), len(callIdx))
	}
	results := map[int]capture.Event{}
	for n, id := range started {
		t, ok := completed[id]
		if !ok {
			continue
		}
		c := m.Events[callIdx[n]]
		e := capture.Event{At: c.At, Kind: capture.KindToolResult, Tool: &capture.Tool{Status: cursorStatus(t)}}
		if c.Tool != nil {
			e.Tool.Name = c.Tool.Name
		}
		if cmd := c.Attrs["command"]; cmd != "" {
			e.Attrs = map[string]string{"command": cmd}
		}
		results[callIdx[n]] = e
	}
	out := make([]capture.Event, 0, len(m.Events)+len(results))
	for i, e := range m.Events {
		out = append(out, e)
		if r, ok := results[i]; ok {
			out = append(out, r)
		}
	}
	for i := range out {
		out[i].Seq = int64(i)
	}
	m.Events = out
	return ""
}

// cursorStatus is ok for a success result with no non-zero exit code, else error.
func cursorStatus(t cursorTool) string {
	for kind, raw := range t.Result {
		if kind != "success" {
			return "error"
		}
		var v struct {
			ExitCode *int `json:"exitCode"`
		}
		if json.Unmarshal(raw, &v) == nil && v.ExitCode != nil && *v.ExitCode != 0 {
			return "error"
		}
	}
	return "ok"
}
