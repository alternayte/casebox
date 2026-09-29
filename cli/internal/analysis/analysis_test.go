package analysis

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var schema = map[string]any{"type": "object", "properties": map[string]any{"intent": map[string]any{"type": "string"}}}

// The Anthropic call forces the one tool, retries a rate limit, and reports the provider's model ID.
func TestAnthropicForcesTheToolAndRetriesARateLimit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "sk-ant-secret" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("request %s with headers %v", r.URL.Path, r.Header)
		}
		var body struct {
			Model      string                      `json:"model"`
			Tools      []struct{ Name string }     `json:"tools"`
			ToolChoice struct{ Type, Name string } `json:"tool_choice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "claude-x" || len(body.Tools) != 1 || body.ToolChoice.Type != "tool" || body.ToolChoice.Name != "label" {
			t.Errorf("body = %+v", body)
		}
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"claude-x-20260901","stop_reason":"tool_use","content":[{"type":"tool_use","name":"label","input":{"intent":"correction"}}]}`))
	}))
	defer srv.Close()

	c := New(Config{Provider: Anthropic, Model: "claude-x", BaseURL: srv.URL, APIKey: "sk-ant-secret"})
	c.Backoff = time.Millisecond
	reply, err := c.Complete(context.Background(), Request{System: "s", User: "u", Tool: "label", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || reply.Model != "claude-x-20260901" || string(reply.JSON) != `{"intent":"correction"}` {
		t.Fatalf("calls = %d, reply = %s %s", calls, reply.Model, reply.JSON)
	}
}

// An OpenAI-compatible call asks for a JSON object; a client error is not retried, and the error
// never carries the key, even when the provider echoes it.
func TestOpenAIErrorsAreNotRetriedAndNeverShowTheKey(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			ResponseFormat struct{ Type string } `json:"response_format"`
			Messages       []struct{ Role, Content string }
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/v1/chat/completions" || body.ResponseFormat.Type != "json_object" || !strings.Contains(body.Messages[0].Content, `"intent"`) {
			t.Errorf("request %s, body %+v", r.URL.Path, body)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: sk-deep-secret"}}`))
	}))
	defer srv.Close()

	c := New(Config{Provider: OpenAI, Model: "deepseek-chat", BaseURL: srv.URL + "/v1", APIKey: "sk-deep-secret"})
	c.Backoff = time.Millisecond
	_, err := c.Complete(context.Background(), Request{System: "s", User: "u", Tool: "label", Schema: schema})
	if err == nil || calls != 1 {
		t.Fatalf("err = %v after %d calls, want one failed call", err, calls)
	}
	if strings.Contains(err.Error(), "sk-deep-secret") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %q", err)
	}
}

func TestTheEnvironmentNamesTheModel(t *testing.T) {
	for _, v := range []string{"CASEBOX_ANALYSIS_PROVIDER", "CASEBOX_ANALYSIS_MODEL", "CASEBOX_ANALYSIS_BASE_URL", "CASEBOX_ANALYSIS_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(v, "")
	}
	if _, err := FromEnv(); err != ErrNotConfigured {
		t.Fatalf("empty environment: %v", err)
	}
	t.Setenv("CASEBOX_ANALYSIS_PROVIDER", "openai")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "CASEBOX_ANALYSIS_MODEL") {
		t.Fatalf("no model: %v", err)
	}
	t.Setenv("CASEBOX_ANALYSIS_MODEL", "gpt-x")
	t.Setenv("OPENAI_API_KEY", "sk-o")
	c, err := FromEnv()
	if err != nil || c.BaseURL != "https://api.openai.com/v1" || c.APIKey != "sk-o" {
		t.Fatalf("config = %+v, %v", c, err)
	}
}
