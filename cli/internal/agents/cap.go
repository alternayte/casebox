package agents

import (
	"bufio"
	"encoding/json"
	"io"

	"github.com/alternayte/casebox/cli/internal/capture"
)

// StreamsUsage reports whether the agent's stdout carries usage while it runs, so WatchCap can
// stop it at the token cap. Claude Code's stream-json reports each model call's usage. Codex's
// --json reports usage only in turn.completed, and codex exec runs one turn, so the usage comes
// at the end; the Cursor CLI reports it only in its result line. For those the cap is checked
// after the run.
func (a Adapter) StreamsUsage() bool {
	return a.spec.Agent == ClaudeCode
}

// CapResult is what WatchCap saw.
type CapResult struct {
	Usage    capture.Usage
	Exceeded bool
}

// WatchCap reads the agent's stdout as it streams and returns as soon as the cumulative tokens
// (as Tokens counts them: cache reads left out) pass limit, with Exceeded set; the caller then stops the
// agent. Otherwise it returns at the end of the stream. It does not drain the rest of r after
// the cap: the caller stops the agent and closes or drains r. A limit of 0 or less is no cap.
func (a Adapter) WatchCap(r io.Reader, limit int64) (CapResult, error) {
	var res CapResult
	perMessage := map[string]capture.Usage{} // Claude Code: the latest usage of each message
	var turns capture.Usage                  // Codex: the sum over completed turns

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		var line struct {
			Type    string `json:"type"`
			Message *struct {
				ID    string          `json:"id"`
				Usage *anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			continue
		}
		switch a.watchFormat() {
		case ClaudeCode:
			switch {
			case line.Type == "assistant" && line.Message != nil && line.Message.Usage != nil:
				// Each content block of a message repeats its usage; the latest counts once.
				prev, u := perMessage[line.Message.ID], line.Message.Usage.usage()
				perMessage[line.Message.ID] = u
				add(&res.Usage, capture.Usage{
					InputTokens: u.InputTokens - prev.InputTokens, OutputTokens: u.OutputTokens - prev.OutputTokens,
					CacheReadTokens: u.CacheReadTokens - prev.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens - prev.CacheWriteTokens,
				})
			case line.Type == "result" && len(line.Usage) > 0:
				// The result's usage is the session's total; it also counts what the per-message
				// lines did not show, so the larger of the two stands.
				var u anthropicUsage
				if json.Unmarshal(line.Usage, &u) == nil && Tokens(u.usage()) > Tokens(res.Usage) {
					res.Usage = u.usage()
				}
			}
		case Codex:
			if line.Type == "turn.completed" && len(line.Usage) > 0 {
				var u struct {
					InputTokens      int64 `json:"input_tokens"`
					CachedTokens     int64 `json:"cached_input_tokens"`
					CacheWriteTokens int64 `json:"cache_write_input_tokens"`
					OutputTokens     int64 `json:"output_tokens"`
				}
				if json.Unmarshal(line.Usage, &u) == nil {
					add(&turns, capture.Usage{
						InputTokens:  max(u.InputTokens-u.CachedTokens-u.CacheWriteTokens, 0),
						OutputTokens: u.OutputTokens, CacheReadTokens: u.CachedTokens, CacheWriteTokens: u.CacheWriteTokens,
					})
					res.Usage = turns
				}
			}
		case CursorCLI:
			if line.Type == "result" && len(line.Usage) > 0 {
				var u struct {
					InputTokens      int64 `json:"inputTokens"`
					OutputTokens     int64 `json:"outputTokens"`
					CacheReadTokens  int64 `json:"cacheReadTokens"`
					CacheWriteTokens int64 `json:"cacheWriteTokens"`
				}
				if json.Unmarshal(line.Usage, &u) == nil {
					res.Usage = capture.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens}
				}
			}
		}
		if limit > 0 && Tokens(res.Usage) > limit {
			res.Exceeded = true
			return res, nil
		}
	}
	return res, scanner.Err()
}

// watchFormat is the format of the agent's stdout; a command template's is unknown.
func (a Adapter) watchFormat() string {
	if a.spec.Agent == CommandCLI {
		return ""
	}
	return a.spec.Agent
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) usage() capture.Usage {
	return capture.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens}
}

func add(sum *capture.Usage, u capture.Usage) {
	sum.InputTokens += u.InputTokens
	sum.OutputTokens += u.OutputTokens
	sum.CacheReadTokens += u.CacheReadTokens
	sum.CacheWriteTokens += u.CacheWriteTokens
}
