// Package analysis calls the team's analysis model: Anthropic's Messages API or any
// OpenAI-compatible chat completions API (OpenAI, DeepSeek, OpenRouter, a company gateway). The
// worker reads the provider, model, base URL and key from its own environment; the server never
// holds the key.
package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Providers.
const (
	Anthropic = "anthropic"
	OpenAI    = "openai"
)

// Config is the analysis model of this host.
type Config struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
}

func (c Config) String() string { return c.Provider + " " + c.Model }

// ErrNotConfigured means the environment names no analysis model.
var ErrNotConfigured error = &cbx.Error{Code: cbx.NoAnalysisModel, Err: errors.New("no analysis model: set CASEBOX_ANALYSIS_PROVIDER (anthropic or openai) and CASEBOX_ANALYSIS_MODEL")}

// FromEnv reads the analysis model from the environment. It returns ErrNotConfigured when neither
// the provider nor the model is set, and a specific error when the setting is incomplete.
func FromEnv() (Config, error) {
	c := Config{
		Provider: strings.ToLower(strings.TrimSpace(os.Getenv("CASEBOX_ANALYSIS_PROVIDER"))),
		Model:    strings.TrimSpace(os.Getenv("CASEBOX_ANALYSIS_MODEL")),
		BaseURL:  strings.TrimRight(strings.TrimSpace(os.Getenv("CASEBOX_ANALYSIS_BASE_URL")), "/"),
		APIKey:   strings.TrimSpace(os.Getenv("CASEBOX_ANALYSIS_API_KEY")),
	}
	if c.Provider == "" && c.Model == "" {
		return c, ErrNotConfigured
	}
	var defaultURL, keyVar string
	switch c.Provider {
	case Anthropic:
		defaultURL, keyVar = "https://api.anthropic.com", "ANTHROPIC_API_KEY"
	case OpenAI:
		defaultURL, keyVar = "https://api.openai.com/v1", "OPENAI_API_KEY"
	case "":
		return c, errors.New("CASEBOX_ANALYSIS_MODEL is set but CASEBOX_ANALYSIS_PROVIDER is not; set it to anthropic or openai")
	default:
		return c, fmt.Errorf("CASEBOX_ANALYSIS_PROVIDER is %q; set it to anthropic or openai (any OpenAI-compatible API)", c.Provider)
	}
	if c.Model == "" {
		return c, errors.New("CASEBOX_ANALYSIS_MODEL is required; there is no default model")
	}
	if c.BaseURL == "" {
		c.BaseURL = defaultURL
	}
	if c.APIKey == "" {
		c.APIKey = strings.TrimSpace(os.Getenv(keyVar))
	}
	if c.APIKey == "" {
		return c, fmt.Errorf("no API key for the analysis model: set CASEBOX_ANALYSIS_API_KEY or %s", keyVar)
	}
	return c, nil
}

// Client calls one analysis model.
type Client struct {
	Config
	HTTP     *http.Client
	Attempts int           // calls per request, retrying 429, 5xx and network errors
	Backoff  time.Duration // the first wait between attempts; it doubles each time
}

// New returns a client with a 2-minute timeout per call and 4 attempts.
func New(c Config) *Client {
	return &Client{Config: c, HTTP: &http.Client{Timeout: 2 * time.Minute}, Attempts: 4, Backoff: 2 * time.Second}
}

// Request asks for one JSON object that matches Schema, a JSON Schema object.
type Request struct {
	System    string
	User      string
	Tool      string // the name of the answer, such as "label"
	Schema    map[string]any
	MaxTokens int
}

// Reply is the model's JSON object and the model ID the provider reported.
type Reply struct {
	JSON  json.RawMessage
	Model string
}

// Complete sends the request and returns the model's JSON object. It does not validate the
// object against the schema; the caller does.
func (c *Client) Complete(ctx context.Context, req Request) (Reply, error) {
	if req.MaxTokens == 0 {
		req.MaxTokens = 1024
	}
	var reply Reply
	var err error
	switch c.Provider {
	case Anthropic:
		reply, err = c.anthropic(ctx, req)
	case OpenAI:
		reply, err = c.openai(ctx, req)
	default:
		return Reply{}, fmt.Errorf("unknown analysis provider %q", c.Provider)
	}
	if err != nil {
		return Reply{}, err
	}
	if reply.Model == "" {
		reply.Model = c.Model
	}
	return reply, nil
}

func (c *Client) anthropic(ctx context.Context, req Request) (Reply, error) {
	body := map[string]any{
		"model":      c.Model,
		"max_tokens": req.MaxTokens,
		"system":     req.System,
		"messages":   []map[string]any{{"role": "user", "content": req.User}},
		"tools": []map[string]any{{
			"name":         req.Tool,
			"description":  "Record the answer.",
			"input_schema": req.Schema,
		}},
		"tool_choice": map[string]any{"type": "tool", "name": req.Tool},
	}
	headers := map[string]string{"x-api-key": c.APIKey, "anthropic-version": "2023-06-01"}
	var resp struct {
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := c.post(ctx, c.BaseURL+"/v1/messages", headers, body, &resp); err != nil {
		return Reply{}, err
	}
	for _, block := range resp.Content {
		if block.Type == "tool_use" && block.Name == req.Tool {
			return Reply{JSON: block.Input, Model: resp.Model}, nil
		}
	}
	return Reply{}, fmt.Errorf("the analysis model returned no %s tool call (stop reason %q)", req.Tool, resp.StopReason)
}

func (c *Client) openai(ctx context.Context, req Request) (Reply, error) {
	schema, err := json.Marshal(req.Schema)
	if err != nil {
		return Reply{}, err
	}
	system := req.System + "\n\nAnswer with one JSON object and nothing else. It must match this JSON schema:\n" + string(schema)
	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": req.User},
		},
		"response_format": map[string]any{"type": "json_object"},
	}
	headers := map[string]string{"Authorization": "Bearer " + c.APIKey}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := c.post(ctx, c.BaseURL+"/chat/completions", headers, body, &resp); err != nil {
		return Reply{}, err
	}
	if len(resp.Choices) == 0 {
		return Reply{}, errors.New("the analysis model returned no choices")
	}
	text := strings.TrimSpace(resp.Choices[0].Message.Content)
	// Some gateways wrap the object in a Markdown fence even in JSON mode.
	text = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```"), "```")
	text = strings.TrimSpace(text)
	if !json.Valid([]byte(text)) {
		return Reply{}, &InvalidOutputError{Reason: "the reply is not JSON (finish reason " + strconv.Quote(resp.Choices[0].FinishReason) + ")"}
	}
	return Reply{JSON: json.RawMessage(text), Model: resp.Model}, nil
}

// InvalidOutputError is a reply the model gave that is not the JSON object asked for.
type InvalidOutputError struct{ Reason string }

func (e *InvalidOutputError) Error() string { return "invalid output: " + e.Reason }

// StatusError is a non-2xx answer from the provider. Message never holds the API key.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("the analysis model answered %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("the analysis model answered %d", e.Status)
}

func (c *Client) post(ctx context.Context, url string, headers map[string]string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	wait := c.Backoff
	attempts := max(c.Attempts, 1)
	for attempt := 1; ; attempt++ {
		retryAfter, err := c.postOnce(ctx, url, headers, data, out)
		if err == nil {
			return nil
		}
		var status *StatusError
		retryable := !errors.As(err, &status) || status.Status == http.StatusTooManyRequests || status.Status >= 500
		if !retryable || attempt >= attempts || ctx.Err() != nil {
			return err
		}
		pause := wait
		if retryAfter > pause {
			pause = min(retryAfter, time.Minute)
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause):
		}
		wait *= 2
	}
}

func (c *Client) postOnce(ctx context.Context, url string, headers map[string]string, data []byte, out any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("reach the analysis model at %s: %s", c.BaseURL, c.scrub(err.Error()))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, fmt.Errorf("read the analysis model's answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var retryAfter time.Duration
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			retryAfter = time.Duration(s) * time.Second
		}
		return retryAfter, &StatusError{Status: resp.StatusCode, Message: c.scrub(problem(raw))}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return 0, fmt.Errorf("the analysis model's answer is not the expected JSON: %w", err)
	}
	return 0, nil
}

// problem reads the error message of an Anthropic or OpenAI error body.
func problem(raw []byte) string {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	msg := ""
	if json.Unmarshal(raw, &body) == nil && len(body.Error) > 0 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body.Error, &e) == nil && e.Message != "" {
			msg = e.Message
		} else {
			_ = json.Unmarshal(body.Error, &msg)
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// scrub removes the API key from text that came back from the network.
func (c *Client) scrub(text string) string {
	if c.APIKey == "" {
		return text
	}
	return strings.ReplaceAll(text, c.APIKey, "[api key]")
}
