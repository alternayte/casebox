package codex

import (
	"strings"
	"testing"

	"github.com/alternayte/casebox/cli/internal/capture"
)

// Steering counts a denial only when a person made it, so the parser must tell a person's
// rejection from Codex's automatic approval review. The golden fixtures hold neither.
func TestDenialsNameWhoDenied(t *testing.T) {
	rollout := strings.Join([]string{
		`{"timestamp":"2026-09-05T10:00:00Z","type":"session_meta","payload":{"id":"s1","cwd":"/work/app","cli_version":"0.153.4","source":"cli"}}`,
		`{"timestamp":"2026-09-05T10:00:01Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{}"}}`,
		`{"timestamp":"2026-09-05T10:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"exec command rejected by user"}}`,
		`{"timestamp":"2026-09-05T10:00:03Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c2","arguments":"{}"}}`,
		`{"timestamp":"2026-09-05T10:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c2","output":"automatic approval review denied the command"}}`,
	}, "\n")
	res, err := Parse(strings.NewReader(rollout))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range res.Events {
		if e.Kind == capture.KindDenial {
			got = append(got, e.Attrs["denial"])
		}
	}
	if strings.Join(got, ",") != "user-rejected,auto-review" {
		t.Fatalf("denials = %v, want user-rejected then auto-review", got)
	}
}
