package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Uninstalling takes out only what casebox init wrote: the person's own env variables, telemetry
// for another backend, and Codex settings stay.
func TestRemovingTelemetryKeepsWhatThePersonWrote(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(claude, []byte(`{"env":{"MY_OWN":"keep"},"model":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tel := Telemetry{Server: "http://localhost:8080", IngestToken: "cbx_ingest_abc", LogPrompts: true}
	if err := SetClaudeTelemetry(claude, tel); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveClaudeTelemetry(claude); err != nil || !removed {
		t.Fatalf("removed = %v, err = %v", removed, err)
	}
	data, _ := os.ReadFile(claude)
	if strings.Contains(string(data), "OTEL") || !strings.Contains(string(data), `"MY_OWN": "keep"`) || !strings.Contains(string(data), `"model": "x"`) {
		t.Fatalf("settings after removal:\n%s", data)
	}

	other := filepath.Join(dir, "other.json")
	if err := os.WriteFile(other, []byte(`{"env":{"OTEL_EXPORTER_OTLP_HEADERS":"Authorization=Bearer theirs"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, _ := RemoveClaudeTelemetry(other); removed {
		t.Fatal("removed telemetry that points at another backend")
	}

	codex := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(codex, []byte("[profile]\nmodel = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetCodexTelemetry(codex, tel); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveCodexTelemetry(codex); err != nil || !removed {
		t.Fatalf("removed = %v, err = %v", removed, err)
	}
	if data, _ := os.ReadFile(codex); string(data) != "[profile]\nmodel = \"x\"\n" {
		t.Fatalf("config.toml after removal:\n%q", data)
	}
}
