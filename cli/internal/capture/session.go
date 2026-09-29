// Package capture holds the canonical session format of docs/specs/capture.md, and the
// redaction, scrubbing and identity marking every source goes through before the spool.
package capture

import "time"

// Session is one agent run.
type Session struct {
	ID           string     `json:"id"`
	Agent        string     `json:"agent"`
	AgentVersion string     `json:"agentVersion,omitempty"`
	Model        string     `json:"model,omitempty"`
	Repo         string     `json:"repo,omitempty"`
	Branch       string     `json:"branch,omitempty"`
	HeadStart    string     `json:"headStart,omitempty"`
	HeadEnd      string     `json:"headEnd,omitempty"`
	StartedAt    time.Time  `json:"startedAt"`
	EndedAt      *time.Time `json:"endedAt,omitempty"`
	Source       string     `json:"source"`
	WorkItem     string     `json:"workItem,omitempty"`
	Person       string     `json:"person"`
}

// Event kinds.
const (
	KindSessionStart = "session_start"
	KindPrompt       = "prompt"
	KindResponse     = "response"
	KindToolCall     = "tool_call"
	KindToolResult   = "tool_result"
	KindInterruption = "interruption"
	KindDenial       = "denial"
	KindRewind       = "rewind"
	KindHumanEdit    = "human_edit"
	KindCompaction   = "compaction"
	KindSessionEnd   = "session_end"
	KindUnknown      = "unknown"
)

// Sequence ranges keep sources apart within one session: import from 0, hooks from 1e9, OTel
// (server side) from 2e9, Entire checkpoints from 3e9.
const (
	HookSeqBase   = 1_000_000_000
	EntireSeqBase = 3_000_000_000
)

// Event is one thing that happened in a session.
type Event struct {
	Seq   int64             `json:"seq"`
	At    time.Time         `json:"at"`
	Kind  string            `json:"kind"`
	Text  string            `json:"text,omitempty"`
	Tool  *Tool             `json:"tool,omitempty"`
	Usage *Usage            `json:"usage,omitempty"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Tool describes a tool call or its result.
type Tool struct {
	Name   string   `json:"name,omitempty"`
	Status string   `json:"status,omitempty"`
	Files  []string `json:"files,omitempty"`
}

// Usage is token use and cost for one model call.
type Usage struct {
	InputTokens      int64    `json:"inputTokens,omitempty"`
	OutputTokens     int64    `json:"outputTokens,omitempty"`
	CacheReadTokens  int64    `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64    `json:"cacheWriteTokens,omitempty"`
	CostUSD          *float64 `json:"costUsd,omitempty"`
}
