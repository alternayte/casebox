// Package hooks installs Casebox's hooks into Claude Code, Codex and Cursor CLI, and handles
// each hook call. A hook call never blocks the agent: it writes a few rows to the spool and moves
// slower work (the transcript import, the upload) to a detached background process.
package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/config"
	"github.com/alternayte/casebox/cli/internal/pipeline"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/spool"
)

// Agents.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
	Cursor     = "cursor-cli"
)

// What a hook call means, whatever the agent calls it.
type action int

const (
	actStart action = iota
	actPrompt
	actTool
	actFileEdit
	actStop
	actEnd
	actIgnore
)

var actions = map[string]map[string]action{
	ClaudeCode: {"SessionStart": actStart, "UserPromptSubmit": actPrompt, "PostToolUse": actTool, "Stop": actStop, "SessionEnd": actEnd},
	Codex:      {"SessionStart": actStart, "UserPromptSubmit": actPrompt, "PostToolUse": actTool, "Stop": actStop, "SessionEnd": actEnd},
	Cursor:     {"sessionStart": actStart, "postToolUse": actTool, "afterFileEdit": actFileEdit, "sessionEnd": actEnd},
}

type payload struct {
	SessionID      string          `json:"session_id"`
	ConversationID string          `json:"conversation_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	WorkspaceRoots []string        `json:"workspace_roots"`
	CursorVersion  string          `json:"cursor_version"`
	Model          string          `json:"model"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	FilePath       string          `json:"file_path"`
	Reason         string          `json:"reason"`
}

// state is what one session's hooks remember between calls.
type state struct {
	Touched []string          `json:"touched"`
	Hashes  map[string]string `json:"hashes"`
}

// Background starts a detached process of this binary with args. The cmd package sets it.
var Background = func(args ...string) error { return nil }

// handle processes one queued hook call.
func handle(ctx context.Context, agent, event string, at time.Time, data []byte) error {
	act, known := actions[agent][event]
	if !known {
		return nil
	}
	var p payload
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	// Cursor also runs the hooks in ~/.claude/settings.json; its sessions are captured by the
	// Cursor hooks, so a Claude Code hook call from Cursor is ignored.
	if agent == ClaudeCode && (p.CursorVersion != "" || p.ConversationID != "" || os.Getenv("CURSOR_VERSION") != "") {
		return nil
	}
	sessionID := p.SessionID
	if agent == Cursor {
		sessionID = p.ConversationID
	}
	cwd := p.Cwd
	if cwd == "" && len(p.WorkspaceRoots) > 0 {
		cwd = p.WorkspaceRoots[0]
	}
	if sessionID == "" || cwd == "" {
		return nil
	}

	r, err := pipeline.Open(ctx, cwd)
	if err != nil {
		return nil // not an enrolled repository: capture nothing
	}
	path, err := config.Path("spool.db")
	if err != nil {
		return err
	}
	s, err := spool.Open(path, spool.DefaultCap)
	if err != nil {
		return err
	}
	defer s.Close()

	now := at.UTC()
	session := r.Session(capture.Session{ID: agent + ":" + sessionID, Agent: agent, Source: "hook", StartedAt: now, Model: p.Model})
	mode := pipeline.Mode()
	key := "hook:" + session.ID
	var st state
	if _, err := s.State(ctx, key, &st); err != nil {
		return err
	}

	add := func(e capture.Event) error {
		e.At = now
		return s.AddHook(ctx, session, r.Events([]capture.Event{e}, mode)[0])
	}

	switch act {
	case actStart:
		session.HeadStart = r.State.Head
		err = add(capture.Event{Kind: capture.KindSessionStart})
	case actPrompt:
		// A file the agent touched that changed between the agent's stop and this prompt was
		// edited by a person.
		if changed := changedFiles(r.Root, st.Hashes); len(changed) > 0 {
			if err := add(capture.Event{Kind: capture.KindHumanEdit, Tool: &capture.Tool{Files: changed}}); err != nil {
				return err
			}
		}
		st.Hashes = nil
	case actTool:
		st.Touched = appendNew(st.Touched, toolFiles(p.ToolInput)...)
	case actFileEdit:
		st.Touched = appendNew(st.Touched, p.FilePath)
	case actStop, actEnd:
		st.Hashes = hashFiles(r.Root, st.Touched)
		if act == actEnd {
			end := now
			session.EndedAt, session.HeadEnd = &end, repo.Current(ctx, r.Root).Head
			err = add(capture.Event{Kind: capture.KindSessionEnd, Attrs: map[string]string{"reason": p.Reason}})
		}
		// The transcript holds the conversation; importing it is slow, so it runs in the background.
		// A Cursor transcript names no directory, so the hook passes the workspace.
		if p.TranscriptPath != "" {
			_ = Background("capture", "transcript", "--agent", agent, "--path", p.TranscriptPath, "--cwd", cwd)
		}
		_ = Background("capture", "upload")
	}
	if err != nil {
		return err
	}
	return s.SetState(ctx, key, st)
}

func toolFiles(input json.RawMessage) []string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"file_path", "path", "notebook_path"} {
		if v, ok := in[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

func appendNew(list []string, items ...string) []string {
	for _, item := range items {
		if item == "" {
			continue
		}
		found := false
		for _, l := range list {
			if l == item {
				found = true
				break
			}
		}
		if !found {
			list = append(list, item)
		}
	}
	return list
}

func hashFiles(root string, files []string) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		path := f
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		out[f] = fileHash(path)
	}
	return out
}

func changedFiles(root string, hashes map[string]string) []string {
	var changed []string
	for f, before := range hashes {
		path := f
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if fileHash(path) != before {
			changed = append(changed, f)
		}
	}
	return changed
}

// fileHash hashes a file; a missing file hashes to "missing", so a deletion counts as a change.
func fileHash(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	if info.Size() > 16<<20 {
		return "size:" + strings.TrimSpace(info.ModTime().UTC().Format(time.RFC3339Nano))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
