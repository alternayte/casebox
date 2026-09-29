// Package entire reads Entire checkpoints from a repository mirror: the per-checkpoint refs
// (refs/entire/checkpoints/<shard>/<id>, Entire 0.10 and later) and the older
// entire/checkpoints/v1 branch. It reads Entire's normalized transcript.jsonl, which has one
// schema for every agent, and links each checkpoint to the code commit whose
// "Entire-Checkpoint:" trailer names it.
package entire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/gitmirror"
)

var (
	ulid     = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	hexID    = regexp.MustCompile(`^[0-9a-f]{12}$`)
	trailer  = regexp.MustCompile(`Entire-Checkpoint:\s*([0-9a-f]{12}|[0-9A-HJKMNP-TV-Z]{26})(?:\s|$)`)
	agentMap = map[string]string{
		"claude-code": "claude-code", "Claude Code": "claude-code",
		"codex": "codex", "Codex": "codex",
		"cursor": "cursor-cli", "Cursor": "cursor-cli",
		"opencode": "opencode", "OpenCode": "opencode",
		"pi": "pi", "Pi": "pi",
		"copilot-cli": "copilot-cli", "Copilot CLI": "copilot-cli",
	}
)

// Session is one agent session from a checkpoint, with the email of the commit author who kept it.
type Session struct {
	Session     capture.Session
	Events      []capture.Event
	AuthorEmail string
	CommitSHA   string
}

type checkpoint struct {
	id     string
	object string // "<commit>:<prefix>" of the checkpoint's tree
}

type sessionMeta struct {
	SessionID  string    `json:"session_id"`
	Agent      string    `json:"agent"`
	Model      string    `json:"model"`
	Branch     string    `json:"branch"`
	CreatedAt  time.Time `json:"created_at"`
	CLIVersion string    `json:"cli_version"`
}

// Read returns the sessions of every checkpoint that a commit since `since` names. A session saved
// in several checkpoints is read once, from its newest checkpoint, which holds the whole session.
func Read(ctx context.Context, m *gitmirror.Mirror, since time.Time) ([]Session, error) {
	commits, err := linkedCommits(ctx, m, since)
	if err != nil || len(commits) == 0 {
		return nil, err
	}
	checkpoints, err := list(ctx, m)
	if err != nil {
		return nil, err
	}

	latest := map[string]Session{}
	for _, cp := range checkpoints {
		commit, linked := commits[cp.id]
		if !linked {
			continue
		}
		dirs, err := m.Git(ctx, "ls-tree", "--name-only", cp.object)
		if err != nil {
			continue
		}
		for _, dir := range strings.Fields(string(dirs)) {
			if _, err := strconv.Atoi(dir); err != nil {
				continue
			}
			s, ok := readSession(ctx, m, cp.object+dir+"/")
			if !ok {
				continue
			}
			s.AuthorEmail, s.CommitSHA = commit.email, commit.sha
			if prev, seen := latest[s.Session.ID]; !seen || len(s.Events) >= len(prev.Events) {
				latest[s.Session.ID] = s
			}
		}
	}
	out := make([]Session, 0, len(latest))
	for _, s := range latest {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session.ID < out[j].Session.ID })
	return out, nil
}

type commitRef struct{ sha, email string }

// linkedCommits maps each checkpoint ID a code commit names to that commit.
func linkedCommits(ctx context.Context, m *gitmirror.Mirror, since time.Time) (map[string]commitRef, error) {
	out, err := m.Git(ctx, "log", "--branches", "--since="+since.Format(time.RFC3339), "--grep=Entire-Checkpoint:", "--format=%H%x00%ae%x00%B%x1e")
	if err != nil {
		return nil, err
	}
	commits := map[string]commitRef{}
	for _, record := range strings.Split(string(out), "\x1e") {
		parts := strings.SplitN(strings.TrimLeft(record, "\n"), "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		for _, match := range trailer.FindAllStringSubmatch(parts[2], -1) {
			commits[match[1]] = commitRef{sha: parts[0], email: parts[1]}
		}
	}
	return commits, nil
}

// list finds checkpoints in both storage formats.
func list(ctx context.Context, m *gitmirror.Mirror) ([]checkpoint, error) {
	var out []checkpoint
	refs, err := m.Git(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "refs/entire/checkpoints/")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		name, commit, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(name, "refs/entire/checkpoints/"), "/")
		// Entire's rule: <shard>/<id>, where the shard is the ID's last two characters.
		if len(parts) != 2 || len(parts[1]) < 2 || parts[0] != parts[1][len(parts[1])-2:] || !(ulid.MatchString(parts[1]) || hexID.MatchString(parts[1])) {
			continue
		}
		out = append(out, checkpoint{id: parts[1], object: commit + ":"})
	}

	const branch = "refs/heads/entire/checkpoints/v1"
	if shards, err := m.Git(ctx, "ls-tree", "-d", "--name-only", branch); err == nil {
		for _, shard := range strings.Fields(string(shards)) {
			rest, err := m.Git(ctx, "ls-tree", "-d", "--name-only", branch+":"+shard)
			if err != nil {
				continue
			}
			for _, tail := range strings.Fields(string(rest)) {
				if id := shard + tail; hexID.MatchString(id) {
					out = append(out, checkpoint{id: id, object: branch + ":" + shard + "/" + tail + "/"})
				}
			}
		}
	}
	return out, nil
}

type compactLine struct {
	Type         string            `json:"type"`
	Agent        string            `json:"agent"`
	TS           time.Time         `json:"ts"`
	InputTokens  int64             `json:"input_tokens"`
	OutputTokens int64             `json:"output_tokens"`
	Content      []json.RawMessage `json:"content"`
}

type compactBlock struct {
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	Result *struct {
		Output string `json:"output"`
		Status string `json:"status"`
	} `json:"result"`
}

func readSession(ctx context.Context, m *gitmirror.Mirror, prefix string) (Session, bool) {
	raw, err := m.Show(ctx, prefix+"metadata.json")
	if err != nil {
		return Session{}, false
	}
	var meta sessionMeta
	if json.Unmarshal(raw, &meta) != nil || meta.SessionID == "" {
		return Session{}, false
	}
	transcript, err := m.Show(ctx, prefix+"transcript.jsonl")
	if err != nil {
		return Session{}, false // before Entire 0.7.8 there is no normalized transcript
	}

	agent := agentMap[meta.Agent]
	var events []capture.Event
	emit := func(e capture.Event) {
		e.Seq = capture.EntireSeqBase + int64(len(events))
		events = append(events, e)
	}
	scanner := bufio.NewScanner(bytes.NewReader(transcript))
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	var first, last time.Time
	for scanner.Scan() {
		var l compactLine
		if json.Unmarshal(scanner.Bytes(), &l) != nil {
			continue
		}
		if a := agentMap[l.Agent]; a != "" && agent == "" {
			agent = a
		}
		if first.IsZero() {
			first = l.TS
		}
		last = l.TS
		switch l.Type {
		case "user":
			var texts []string
			for _, c := range l.Content {
				var b compactBlock
				if json.Unmarshal(c, &b) == nil && b.Text != "" {
					texts = append(texts, b.Text)
				}
			}
			if len(texts) > 0 {
				emit(capture.Event{At: l.TS, Kind: capture.KindPrompt, Text: strings.Join(texts, "\n")})
			}
		case "assistant":
			var texts []string
			var tools []capture.Event
			for _, c := range l.Content {
				var b compactBlock
				if json.Unmarshal(c, &b) != nil {
					continue
				}
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) != "" {
						texts = append(texts, b.Text)
					}
				case "tool_use":
					tool := &capture.Tool{Name: b.Name, Files: files(b.Input)}
					tools = append(tools, capture.Event{At: l.TS, Kind: capture.KindToolCall, Tool: tool})
					if b.Result != nil {
						status := "ok"
						if b.Result.Status == "error" {
							status = "error"
						}
						tools = append(tools, capture.Event{At: l.TS, Kind: capture.KindToolResult, Text: b.Result.Output, Tool: &capture.Tool{Name: b.Name, Status: status}})
					}
				}
			}
			usage := &capture.Usage{InputTokens: l.InputTokens, OutputTokens: l.OutputTokens}
			if len(texts) > 0 {
				emit(capture.Event{At: l.TS, Kind: capture.KindResponse, Text: strings.Join(texts, "\n\n"), Usage: usage})
				usage = nil
			}
			for i, t := range tools {
				if i == 0 && usage != nil {
					t.Usage = usage
				}
				emit(t)
			}
		default:
			emit(capture.Event{At: l.TS, Kind: capture.KindUnknown, Attrs: map[string]string{"nativeType": l.Type}})
		}
	}
	if agent == "" || len(events) == 0 {
		return Session{}, false
	}
	started := meta.CreatedAt
	if started.IsZero() || (!first.IsZero() && first.Before(started)) {
		started = first
	}
	end := last
	return Session{Session: capture.Session{
		ID: agent + ":" + meta.SessionID, Agent: agent, AgentVersion: "", Model: meta.Model, Branch: meta.Branch,
		StartedAt: started.UTC(), EndedAt: &end, Source: "entire",
	}, Events: events}, true
}

func files(input json.RawMessage) []string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"file_path", "path", "filePath", "notebook_path"} {
		if v, ok := in[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}
