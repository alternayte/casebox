package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Telemetry is where the agents send native OpenTelemetry: the server's OTLP/HTTP endpoints,
// authorized with this machine's ingest token.
type Telemetry struct {
	Server      string
	IngestToken string
	LogPrompts  bool // prompt mode redacted or full
}

// ClaudeEnv is what casebox init sets in the env block of ~/.claude/settings.json.
func (t Telemetry) ClaudeEnv() map[string]string {
	prompts := "0"
	if t.LogPrompts {
		prompts = "1"
	}
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_LOGS_EXPORTER":           "otlp",
		"OTEL_METRICS_EXPORTER":        "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":  "http/json",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  strings.TrimRight(t.Server, "/"),
		"OTEL_EXPORTER_OTLP_HEADERS":   "Authorization=Bearer " + t.IngestToken,
		"OTEL_LOG_USER_PROMPTS":        prompts,
	}
}

// SetClaudeTelemetry writes the telemetry variables into the env block and keeps everything else.
func SetClaudeTelemetry(path string, t Telemetry) error {
	doc, err := load(path)
	if err != nil {
		return err
	}
	env := newObject()
	if _, err := doc.get("env", env); err != nil {
		return fmt.Errorf("%s: env is not an object: %w", path, err)
	}
	for k, v := range t.ClaudeEnv() {
		if err := env.set(k, v); err != nil {
			return err
		}
	}
	if err := doc.set("env", env); err != nil {
		return err
	}
	return save(path, doc)
}

const (
	codexBegin = "# casebox:begin (written by casebox init; casebox removes this block when it uninstalls)"
	codexEnd   = "# casebox:end"
)

var (
	codexOwnBlock = regexp.MustCompile(`(?s)\n?` + regexp.QuoteMeta(codexBegin) + `.*?` + regexp.QuoteMeta(codexEnd) + `\n?`)
	codexOtel     = regexp.MustCompile(`(?m)^\s*(\[otel(\.[^\]]*)?\]|otel\s*[.=])`)
)

// ErrCodexOtelTaken means config.toml already has an [otel] table Casebox did not write.
var ErrCodexOtelTaken = errors.New("~/.codex/config.toml already has an [otel] table; point its exporter at the Casebox server by hand, or remove it and run casebox init again")

// SetCodexTelemetry appends Casebox's [otel] block to ~/.codex/config.toml, or replaces the block
// it wrote before. It never edits a table the person wrote.
func SetCodexTelemetry(path string, t Telemetry) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := codexOwnBlock.ReplaceAllString(string(data), "\n")
	if codexOtel.MatchString(text) {
		return ErrCodexOtelTaken
	}
	server := strings.TrimRight(t.Server, "/")
	header := fmt.Sprintf(`{ authorization = %q }`, "Bearer "+t.IngestToken)
	block := strings.Join([]string{
		codexBegin,
		"[otel]",
		fmt.Sprintf(`exporter = { otlp-http = { endpoint = %q, protocol = "json", headers = %s } }`, server+"/v1/logs", header),
		fmt.Sprintf(`metrics_exporter = { otlp-http = { endpoint = %q, protocol = "json", headers = %s } }`, server+"/v1/metrics", header),
		fmt.Sprintf("log_user_prompt = %t", t.LogPrompts),
		codexEnd,
	}, "\n")
	text = strings.TrimRight(text, "\n")
	if text != "" {
		text += "\n\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text+block+"\n"), 0o600)
}

var codexTrusted = regexp.MustCompile(`\[hooks\.state\."([^"]+)"\]`)

// CodexUntrusted lists the Casebox hook events Codex has not trusted yet. Codex runs a new hook
// only after the person approves it in its /hooks screen, and records the approval under
// "<hooks file>:<event>:<group index>:<handler index>".
func CodexUntrusted(t Target, configPath string) ([]string, error) {
	data, _ := os.ReadFile(configPath)
	trusted := map[string]bool{}
	for _, m := range codexTrusted.FindAllStringSubmatch(string(data), -1) {
		trusted[m[1]] = true
	}
	doc, err := load(t.Path)
	if err != nil {
		return nil, err
	}
	hooks := newObject()
	if _, err := doc.get("hooks", hooks); err != nil {
		return nil, err
	}
	var missing []string
	for _, event := range t.Events {
		var groups []map[string]any
		_, _ = hooks.get(event, &groups)
		ok := false
		for g, group := range groups {
			for h, handler := range handlers(t, group) {
				if isOwn(handler) && trusted[fmt.Sprintf("%s:%s:%d:%d", t.Path, snake(event), g, h)] {
					ok = true
				}
			}
		}
		if !ok {
			missing = append(missing, event)
		}
	}
	return missing, nil
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
