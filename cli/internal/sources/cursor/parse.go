// Package cursor parses Cursor CLI transcripts (~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl).
// They hold the conversation but no model, no usage and no times except the <timestamp> Cursor
// puts in each prompt, with minute precision; events after a prompt take its time.
package cursor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/capture"
)

// Agent is the canonical agent name.
const Agent = "cursor-cli"

type line struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Status  string `json:"status"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

var (
	userQuery = regexp.MustCompile(`(?s)<user_query>\s*(.*?)\s*</user_query>`)
	timestamp = regexp.MustCompile(`<timestamp>([^<]+)</timestamp>`)
	// "Tuesday, Sep 29, 2026, 8:03 AM (UTC+2)"
	stampParts = regexp.MustCompile(`^\w+, (\w{3} \d{1,2}, \d{4}, \d{1,2}:\d{2} [AP]M) \(UTC([+-]\d{1,2})?(?::(\d{2}))?\)$`)
)

// Parse reads one transcript. The session ID is the transcript's file name, which is the
// conversation_id Cursor gives its hooks. fallback times events before the first timestamp.
func Parse(r io.Reader, path string, fallback time.Time) (capture.Session, []capture.Event, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	session := capture.Session{ID: Agent + ":" + id, Agent: Agent, Source: "import", StartedAt: fallback.UTC()}
	var events []capture.Event
	at := fallback.UTC()
	first := true
	emit := func(e capture.Event) {
		e.Seq, e.At = int64(len(events)), at
		events = append(events, e)
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 1; scanner.Scan(); n++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var l line
		if err := json.Unmarshal(scanner.Bytes(), &l); err != nil {
			return session, nil, fmt.Errorf("line %d: %w", n, err)
		}
		switch {
		case l.Type == "turn_ended":
			if l.Status == "aborted" {
				emit(capture.Event{Kind: capture.KindInterruption, Attrs: map[string]string{"during": "turn"}})
			}
		case l.Role == "user":
			var texts []string
			for _, c := range l.Message.Content {
				if c.Type != "text" {
					continue
				}
				if m := timestamp.FindStringSubmatch(c.Text); m != nil {
					if t, ok := parseStamp(m[1]); ok {
						at = t
						if first {
							session.StartedAt = t
						}
					}
				}
				if m := userQuery.FindStringSubmatch(c.Text); m != nil {
					texts = append(texts, m[1])
				}
			}
			first = false
			if len(texts) > 0 {
				emit(capture.Event{Kind: capture.KindPrompt, Text: strings.Join(texts, "\n")})
			}
		case l.Role == "assistant":
			var texts []string
			var tools []capture.Event
			for _, c := range l.Message.Content {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						texts = append(texts, c.Text)
					}
				case "tool_use":
					tools = append(tools, capture.Event{Kind: capture.KindToolCall, Tool: &capture.Tool{Name: c.Name, Files: files(c.Input)}})
				}
			}
			if len(texts) > 0 {
				emit(capture.Event{Kind: capture.KindResponse, Text: strings.Join(texts, "\n\n")})
			}
			for _, t := range tools {
				emit(t)
			}
		default:
			emit(capture.Event{Kind: capture.KindUnknown, Attrs: map[string]string{"nativeType": l.Type + l.Role}})
		}
	}
	if err := scanner.Err(); err != nil {
		return session, nil, err
	}
	end := at
	session.EndedAt = &end
	return session, events, nil
}

func parseStamp(s string) (time.Time, bool) {
	m := stampParts.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return time.Time{}, false
	}
	offset := 0
	if m[2] != "" {
		var h int
		fmt.Sscanf(m[2], "%d", &h)
		offset = h * 3600
		if m[3] != "" {
			var mins int
			fmt.Sscanf(m[3], "%d", &mins)
			if h < 0 {
				mins = -mins
			}
			offset += mins * 60
		}
	}
	t, err := time.ParseInLocation("Jan 2, 2006, 3:04 PM", m[1], time.FixedZone("", offset))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func files(input json.RawMessage) []string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"path", "file_path", "target_file"} {
		if v, ok := in[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}
