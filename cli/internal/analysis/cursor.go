package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/config"
)

// cursorAgentPath finds the Cursor CLI on PATH: cursor-agent, or its newer name agent.
func cursorAgentPath() string {
	for _, name := range []string{"cursor-agent", "agent"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// cursorResult is the object `cursor-agent -p --output-format json` prints when it succeeds.
type cursorResult struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// cursorAgent asks the Cursor CLI in print mode, read-only ("ask" mode), with the prompt on stdin.
// It runs in its own empty workspace under the Casebox home, so the agent sees no repository and
// its transcripts never land in an enrolled repository's capture. BaseURL holds the binary's path.
func (c *Client) cursorAgent(ctx context.Context, req Request) (Reply, error) {
	schema, err := json.Marshal(req.Schema)
	if err != nil {
		return Reply{}, err
	}
	workspace, err := config.Path("analysis-workspace")
	if err != nil {
		return Reply{}, err
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return Reply{}, fmt.Errorf("create %s: %w", workspace, err)
	}
	prompt := req.System + jsonInstruction + string(schema) + "\n\n---\n\n" + req.User
	args := []string{"-p", "--output-format", "json", "--mode", "ask", "--trust"}
	if c.Model != "" && c.Model != "auto" {
		args = append(args, "--model", c.Model)
	}

	wait := c.Backoff
	attempts := max(c.Attempts, 1)
	for attempt := 1; ; attempt++ {
		out, err := c.runCursorAgent(ctx, workspace, args, prompt)
		if err == nil {
			var res cursorResult
			if jerr := json.Unmarshal(out, &res); jerr != nil {
				return Reply{}, &InvalidOutputError{Reason: "cursor-agent printed no JSON result"}
			}
			if res.IsError || res.Type != "result" {
				err = fmt.Errorf("cursor-agent answered %s/%s: %s", res.Type, res.Subtype, clip(res.Result))
			} else {
				text := unfence(res.Result)
				if !json.Valid([]byte(text)) {
					return Reply{}, &InvalidOutputError{Reason: "the reply is not JSON: " + clip(text)}
				}
				return Reply{JSON: json.RawMessage(text), Model: CursorAgent + "/" + c.Model}, nil
			}
		}
		if attempt >= attempts || ctx.Err() != nil {
			return Reply{}, err
		}
		select {
		case <-ctx.Done():
			return Reply{}, err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func (c *Client) runCursorAgent(ctx context.Context, workspace string, args []string, prompt string) ([]byte, error) {
	timeout := 3 * time.Minute
	if c.HTTP != nil && c.HTTP.Timeout > 0 {
		timeout = c.HTTP.Timeout + time.Minute
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(callCtx, c.BaseURL, args...)
	cmd.Dir = workspace
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("cursor-agent did not answer within %s", timeout)
		}
		return nil, fmt.Errorf("cursor-agent failed (%v): %s", err, clip(lastLine(stderr.String(), stdout.String())))
	}
	return stdout.Bytes(), nil
}

// lastLine is the last non-empty line of the first text that has one: the CLI's error message.
func lastLine(texts ...string) string {
	for _, t := range texts {
		lines := strings.Split(strings.TrimSpace(t), "\n")
		if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
			return last
		}
	}
	return "no output"
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
