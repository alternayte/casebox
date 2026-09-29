package capture

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRedactionCorpus(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/redaction-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Name    string   `json:"name"`
			Input   string   `json:"input"`
			Absent  []string `json:"absent"`
			Present []string `json:"present"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	r, err := NewRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			out := r.Redact(c.Input)
			for _, a := range c.Absent {
				if strings.Contains(out, a) {
					t.Errorf("%q still holds %q", out, a)
				}
			}
			for _, p := range c.Present {
				if !strings.Contains(out, p) {
					t.Errorf("%q lost %q", out, p)
				}
			}
		})
	}
}

func TestTeamRulesRedactToo(t *testing.T) {
	r, err := NewRedactor([]string{`ACME-[0-9]{6}`})
	if err != nil {
		t.Fatal(err)
	}
	if out := r.Redact("licence ACME-123456 ok"); out != "licence [redacted:team] ok" {
		t.Fatalf("got %q", out)
	}
}

func TestIdentityMarking(t *testing.T) {
	m := NewMarker([]string{"Ada Lovelace", "grace", "Al"})
	cases := []struct{ in, want string }{
		{"mail ada@example.com now", "mail ⟦cbx:email:ada@example.com⟧ now"},
		{"ask @octocat about it", "ask ⟦cbx:github:octocat⟧ about it"},
		{"Ada Lovelace wrote it", "⟦cbx:name:Ada Lovelace⟧ wrote it"},
		{"Author: Ada Lovelace <ada@example.com>", "Author: ⟦cbx:name:Ada Lovelace⟧ <⟦cbx:email:ada@example.com⟧>"},
		{"grace fixed it", "⟦cbx:name:grace⟧ fixed it"},
		{"@Override public void run()", "⟦cbx:github:Override⟧ public void run()"},
		{"@pytest.fixture and @app.route('/')", "@pytest.fixture and @app.route('/')"},
		{"Algebra stays", "Algebra stays"},
		{"already ⟦cbx:email:x@y.io⟧ marked", "already ⟦cbx:email:x@y.io⟧ marked"},
	}
	for _, c := range cases {
		if got := m.MarkText(c.in); got != c.want {
			t.Errorf("MarkText(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
