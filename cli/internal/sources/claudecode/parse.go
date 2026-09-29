// Package claudecode parses Claude Code transcripts (~/.claude/projects/<project>/<session>.jsonl)
// into the canonical session format. There is no published schema; docs/specs/capture.md and the
// golden files pin what this parser relies on.
package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
)

// Agent is the canonical agent name.
const Agent = "claude-code"

const (
	interruptText        = "[Request interrupted by user]"
	interruptForToolText = "[Request interrupted by user for tool use]"
	rejectedPrefix       = "The user doesn't want to proceed with this tool use."
	deniedPrefix         = "Permission for this command was denied"
)

// Line types that carry no conversation. They are skipped on purpose; any other unknown type
// becomes an unknown event.
var ignored = map[string]bool{
	"attachment": true, "mode": true, "atis-latch": true, "last-prompt": true, "ai-title": true,
	"queue-operation": true, "bridge-session": true, "pr-link": true, "permission-mode": true,
	"file-history-snapshot": true, "file-history-delta": true, "custom-title": true, "agent-name": true,
	"cost-state": true, "continued-in": true, "summary": true,
}

type line struct {
	Type             string                 `json:"type"`
	Subtype          string                 `json:"subtype"`
	UUID             string                 `json:"uuid"`
	ParentUUID       *string                `json:"parentUuid"`
	SessionID        string                 `json:"sessionId"`
	Timestamp        time.Time              `json:"timestamp"`
	Cwd              string                 `json:"cwd"`
	GitBranch        string                 `json:"gitBranch"`
	Version          string                 `json:"version"`
	IsSidechain      bool                   `json:"isSidechain"`
	IsMeta           bool                   `json:"isMeta"`
	IsCompactSummary bool                   `json:"isCompactSummary"`
	ToolDenialKind   string                 `json:"toolDenialKind"`
	Origin           *struct{ Kind string } `json:"origin"`
	Message          json.RawMessage        `json:"message"`
	CompactMetadata  *struct {
		Trigger    string `json:"trigger"`
		PreTokens  int64  `json:"preTokens"`
		PostTokens int64  `json:"postTokens"`
	} `json:"compactMetadata"`
}

type message struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Result is one parsed transcript. Cwd is the directory the session ran in, before scrubbing,
// so the importer can find its repository.
type Result struct {
	Session capture.Session
	Cwd     string
	Events  []capture.Event
}

// Parse reads one main transcript. Subagent transcripts (isSidechain) are skipped: steering
// happens in the main conversation.
func Parse(r io.Reader) (Result, error) {
	p := parser{tools: map[string]string{}, seen: map[string]int{}}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 1; scanner.Scan(); n++ {
		raw := scanner.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			return Result{}, fmt.Errorf("line %d: %w", n, err)
		}
		p.add(l)
	}
	if err := scanner.Err(); err != nil {
		return Result{}, err
	}
	p.flush()
	if p.session.ID == "" {
		return Result{}, fmt.Errorf("the transcript names no session")
	}
	if p.last.IsZero() {
		p.last = p.session.StartedAt
	}
	end := p.last
	p.session.EndedAt = &end
	return Result{Session: p.session, Cwd: p.cwd, Events: p.events}, nil
}

type parser struct {
	session capture.Session
	cwd     string
	events  []capture.Event
	last    time.Time
	tools   map[string]string // tool_use id → tool name
	seen    map[string]int    // uuid → number of human prompts before it
	prompts int

	// The assistant message being assembled: one API message spans several lines.
	msgID    string
	msgAt    time.Time
	msgText  []string
	msgTools []capture.Event
	msgUsage *capture.Usage
}

func (p *parser) emit(e capture.Event) {
	e.Seq = int64(len(p.events))
	p.events = append(p.events, e)
}

func (p *parser) add(l line) {
	if l.IsSidechain {
		return
	}
	if l.SessionID != "" && p.session.ID == "" {
		p.session = capture.Session{ID: Agent + ":" + l.SessionID, Agent: Agent, Source: "import"}
	}
	if !l.Timestamp.IsZero() {
		if p.session.StartedAt.IsZero() {
			p.session.StartedAt = l.Timestamp.UTC()
		}
		p.last = l.Timestamp.UTC()
	}
	if l.Cwd != "" && p.cwd == "" {
		p.cwd = l.Cwd
	}
	if l.GitBranch != "" && p.session.Branch == "" {
		p.session.Branch = l.GitBranch
	}
	if l.Version != "" {
		p.session.AgentVersion = l.Version
	}

	switch {
	case ignored[l.Type]:
		return
	case l.Type == "assistant":
		p.assistant(l)
	case l.Type == "user":
		p.flush()
		p.user(l)
	case l.Type == "system":
		p.flush()
		if l.Subtype == "compact_boundary" {
			e := capture.Event{At: l.Timestamp.UTC(), Kind: capture.KindCompaction, Attrs: map[string]string{}}
			if m := l.CompactMetadata; m != nil {
				e.Attrs["trigger"] = m.Trigger
				e.Attrs["preTokens"] = fmt.Sprint(m.PreTokens)
				e.Attrs["postTokens"] = fmt.Sprint(m.PostTokens)
			}
			p.emit(e)
		}
	default:
		p.flush()
		p.emit(capture.Event{At: l.Timestamp.UTC(), Kind: capture.KindUnknown, Attrs: map[string]string{"nativeType": l.Type}})
	}
	if l.UUID != "" {
		if _, ok := p.seen[l.UUID]; !ok {
			p.seen[l.UUID] = p.prompts
		}
	}
}

func (p *parser) assistant(l line) {
	var m message
	if json.Unmarshal(l.Message, &m) != nil {
		return
	}
	if m.ID != p.msgID {
		p.flush()
		p.msgID, p.msgAt = m.ID, l.Timestamp.UTC()
	}
	synthetic := m.Model == "<synthetic>"
	if m.Model != "" && !synthetic {
		p.session.Model = m.Model
	}
	if m.Usage != nil && !synthetic {
		// The last line of a message carries its final usage.
		p.msgUsage = &capture.Usage{
			InputTokens: m.Usage.InputTokens, OutputTokens: m.Usage.OutputTokens,
			CacheReadTokens: m.Usage.CacheReadInputTokens, CacheWriteTokens: m.Usage.CacheCreationInputTokens,
		}
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				p.msgText = append(p.msgText, b.Text)
			}
		case "tool_use":
			p.tools[b.ID] = b.Name
			p.msgTools = append(p.msgTools, capture.Event{
				At: l.Timestamp.UTC(), Kind: capture.KindToolCall,
				Tool: &capture.Tool{Name: b.Name, Files: files(b.Input)},
			})
		}
	}
}

// flush emits the assistant message being assembled: its text as one response, then its tool
// calls. The usage goes on the first event, so each API call counts once.
func (p *parser) flush() {
	if p.msgID == "" {
		return
	}
	var out []capture.Event
	if len(p.msgText) > 0 {
		out = append(out, capture.Event{At: p.msgAt, Kind: capture.KindResponse, Text: strings.Join(p.msgText, "\n\n")})
	}
	out = append(out, p.msgTools...)
	if len(out) > 0 {
		out[0].Usage = p.msgUsage
	}
	for _, e := range out {
		p.emit(e)
	}
	p.msgID, p.msgText, p.msgTools, p.msgUsage = "", nil, nil, nil
}

func (p *parser) user(l line) {
	var m message
	if json.Unmarshal(l.Message, &m) != nil {
		return
	}
	at := l.Timestamp.UTC()

	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		p.userText(l, at, text)
		return
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_result":
			output := resultText(b.Content)
			e := capture.Event{At: at, Kind: capture.KindToolResult, Tool: &capture.Tool{Name: p.tools[b.ToolUseID], Status: "ok"}, Text: output}
			switch {
			case l.ToolDenialKind != "" || strings.HasPrefix(output, rejectedPrefix) || strings.HasPrefix(output, deniedPrefix):
				e.Kind, e.Tool.Status, e.Text = capture.KindDenial, "denied", ""
				e.Attrs = map[string]string{"denial": denialKind(l.ToolDenialKind, output)}
			case b.IsError:
				e.Tool.Status = "error"
			}
			p.emit(e)
		}
	}
	if len(texts) > 0 {
		p.userText(l, at, strings.Join(texts, "\n"))
	}
}

func (p *parser) userText(l line, at time.Time, text string) {
	trimmed := strings.TrimSpace(text)
	switch {
	case trimmed == interruptText:
		p.emit(capture.Event{At: at, Kind: capture.KindInterruption, Attrs: map[string]string{"during": "response"}})
	case trimmed == interruptForToolText:
		p.emit(capture.Event{At: at, Kind: capture.KindInterruption, Attrs: map[string]string{"during": "tool"}})
	case l.IsMeta || l.IsCompactSummary:
		return
	case l.Origin != nil && l.Origin.Kind != "human":
		// Task notifications, coordinators and auto-continuations are not a person.
		p.emit(capture.Event{At: at, Kind: capture.KindUnknown, Attrs: map[string]string{"nativeType": "user:" + l.Origin.Kind}})
	default:
		// A prompt whose parent came before the previous prompt goes back past the last turn: a rewind.
		if l.ParentUUID != nil {
			if before, ok := p.seen[*l.ParentUUID]; ok && before < p.prompts {
				p.emit(capture.Event{At: at, Kind: capture.KindRewind})
			}
		}
		p.prompts++
		p.emit(capture.Event{At: at, Kind: capture.KindPrompt, Text: text})
	}
}

func denialKind(kind, output string) string {
	switch {
	case kind != "":
		return kind
	case strings.HasPrefix(output, deniedPrefix):
		return "permission-rule"
	default:
		return "user-rejected"
	}
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []block
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// files returns the paths a tool call names, for the tool's files list.
func files(input json.RawMessage) []string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	var out []string
	for _, key := range []string{"file_path", "path", "notebook_path"} {
		if v, ok := in[key].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}
