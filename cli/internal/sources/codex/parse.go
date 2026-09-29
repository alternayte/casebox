// Package codex parses Codex CLI rollout files (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl)
// into the canonical session format. It reads both 0.153 history modes: legacy, which persists
// user_message events, and paginated, which persists item_completed events instead.
package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
)

// Agent is the canonical agent name.
const Agent = "codex"

type line struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type payload struct {
	Type string `json:"type"`

	// session_meta
	ID         string          `json:"id"`
	Cwd        string          `json:"cwd"`
	CLIVersion string          `json:"cli_version"`
	Source     json.RawMessage `json:"source"`
	Git        *struct {
		CommitHash    string `json:"commit_hash"`
		Branch        string `json:"branch"`
		RepositoryURL string `json:"repository_url"`
	} `json:"git"`

	// turn_context
	TurnID string `json:"turn_id"`
	Model  string `json:"model"`

	// response_item
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`

	// event_msg
	Message string          `json:"message"`
	Reason  string          `json:"reason"`
	Item    json.RawMessage `json:"item"`
	Info    *struct {
		LastTokenUsage *usage `json:"last_token_usage"`
	} `json:"info"`

	// token_usage_record
	Usage *usage `json:"usage"`
}

type usage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
}

// Result is one parsed rollout. Cwd is the directory the session ran in, before scrubbing.
// Subagent is true for review and guardian threads, which the importer skips.
type Result struct {
	Session  capture.Session
	Cwd      string
	Remote   string
	Subagent bool
	Events   []capture.Event
}

// ErrUnsupportedFormat marks a rollout in the pre-envelope format of 2024, which has no
// session_meta line. The importer counts and skips such files.
var ErrUnsupportedFormat = errors.New("the rollout uses the pre-2025 format")

var patchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add|Delete) File: (.+)$`)

// Parse reads one rollout file.
func Parse(r io.Reader) (Result, error) {
	var res Result
	calls := map[string]string{} // call_id → tool name
	var last time.Time
	recordUsage := false // 0.153 writes token_usage_record; older versions only token_count
	var pendingUsage *capture.Usage

	emit := func(e capture.Event) {
		e.Seq = int64(len(res.Events))
		if pendingUsage != nil && (e.Kind == capture.KindResponse || e.Kind == capture.KindToolCall) {
			e.Usage, pendingUsage = pendingUsage, nil
		}
		res.Events = append(res.Events, e)
	}

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
		if l.Type == "" {
			if res.Session.ID == "" {
				return Result{}, ErrUnsupportedFormat
			}
			continue
		}
		var p payload
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			return Result{}, fmt.Errorf("line %d: %w", n, err)
		}
		at := l.Timestamp.UTC()
		if !at.IsZero() {
			last = at
		}

		switch l.Type {
		case "session_meta":
			if res.Session.ID != "" {
				continue
			}
			res.Session = capture.Session{ID: Agent + ":" + p.ID, Agent: Agent, AgentVersion: p.CLIVersion, StartedAt: at, Source: "import"}
			res.Cwd = p.Cwd
			if p.Git != nil {
				res.Session.Branch, res.Session.HeadStart, res.Remote = p.Git.Branch, p.Git.CommitHash, p.Git.RepositoryURL
			}
			var source string
			res.Subagent = json.Unmarshal(p.Source, &source) != nil || (source != "" && source != "cli" && source != "exec")
		case "turn_context":
			if p.Model != "" {
				res.Session.Model = p.Model
			}
		case "token_usage_record":
			recordUsage = true
			if p.Usage != nil {
				pendingUsage = toUsage(p.Usage)
			}
		case "compacted":
			emit(capture.Event{At: at, Kind: capture.KindCompaction})
		case "response_item":
			switch p.Type {
			case "message":
				if p.Role == "assistant" {
					if text := contentText(p.Content); text != "" {
						emit(capture.Event{At: at, Kind: capture.KindResponse, Text: text})
					}
				}
			case "function_call", "custom_tool_call", "local_shell_call":
				calls[p.CallID] = p.Name
				var files []string
				for _, m := range patchFile.FindAllStringSubmatch(p.Input+p.Arguments, -1) {
					files = append(files, strings.TrimSpace(m[1]))
				}
				emit(capture.Event{At: at, Kind: capture.KindToolCall, Tool: &capture.Tool{Name: p.Name, Files: files}})
			case "function_call_output", "custom_tool_call_output":
				output := outputText(p.Output)
				e := capture.Event{At: at, Kind: capture.KindToolResult, Tool: &capture.Tool{Name: calls[p.CallID], Status: "ok"}, Text: output}
				if denied(output) {
					e.Kind, e.Tool.Status, e.Text = capture.KindDenial, "denied", ""
				}
				emit(e)
			}
		case "event_msg":
			switch p.Type {
			case "user_message":
				emit(capture.Event{At: at, Kind: capture.KindPrompt, Text: p.Message})
			case "item_completed":
				var item struct {
					Type    string `json:"type"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				}
				if json.Unmarshal(p.Item, &item) == nil && item.Type == "UserMessage" {
					var texts []string
					for _, c := range item.Content {
						if c.Type == "text" {
							texts = append(texts, c.Text)
						}
					}
					emit(capture.Event{At: at, Kind: capture.KindPrompt, Text: strings.Join(texts, "\n")})
				}
			case "turn_aborted":
				if p.Reason == "interrupted" {
					emit(capture.Event{At: at, Kind: capture.KindInterruption, Attrs: map[string]string{"during": "turn"}})
				}
			case "token_count":
				if !recordUsage && p.Info != nil && p.Info.LastTokenUsage != nil {
					pendingUsage = toUsage(p.Info.LastTokenUsage)
				}
			case "context_compacted":
				emit(capture.Event{At: at, Kind: capture.KindCompaction})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, err
	}
	if res.Session.ID == "" {
		return Result{}, fmt.Errorf("the rollout has no session_meta line")
	}
	end := last
	res.Session.EndedAt = &end
	return res, nil
}

func toUsage(u *usage) *capture.Usage {
	return &capture.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CachedInputTokens, CacheWriteTokens: u.CacheWriteInputTokens}
}

func denied(output string) bool {
	for _, marker := range []string{"rejected by user", "automatic approval review denied"} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

func contentText(raw json.RawMessage) string {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if (p.Type == "output_text" || p.Type == "text") && strings.TrimSpace(p.Text) != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func outputText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return contentText(raw)
}
