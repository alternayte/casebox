package agents

import (
	"testing"

	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/repo"
)

func TestProcessChecks(t *testing.T) {
	call := func(name, command string, files ...string) capture.Event {
		e := capture.Event{Kind: capture.KindToolCall, Tool: &capture.Tool{Name: name, Files: files}}
		if command != "" {
			e.Attrs = map[string]string{"command": command}
		}
		return e
	}
	result := func(status, command string) capture.Event {
		return capture.Event{Kind: capture.KindToolResult, Tool: &capture.Tool{Name: "Bash", Status: status}, Attrs: map[string]string{"command": command}}
	}
	response := capture.Event{Kind: capture.KindResponse, Text: "done"}
	tests := []string{"go test ./...", "dotnet test server/Casebox.slnx -c Release"}

	cases := []struct {
		name        string
		events      []capture.Event
		ran, edited bool
	}{
		{"tests then done", []capture.Event{call("Bash", "go test ./..."), result("ok", "go test ./..."), response}, true, false},
		{"a narrower run of the same command", []capture.Event{call("Bash", "cd pkg && go test -run TestX ./pkg"), response}, true, false},
		{"another recipe command", []capture.Event{call("Bash", "(dotnet test server/Casebox.Tests)"), response}, true, false},
		{"tests only after the last response", []capture.Event{response, call("Bash", "go test ./...")}, false, false},
		{"no response at all", []capture.Event{call("Bash", "go test ./...")}, false, false},
		{"a command that is not a test", []capture.Event{call("Bash", "go vet ./..."), call("Bash", "go testify"), response}, false, false},
		{"edit of a test after a failed test run", []capture.Event{
			call("Bash", "go test ./..."), result("error", "go test ./..."),
			call("Edit", "", "/workspace/parse/parse_test.go"), response,
		}, true, true},
		{"edit of a test before the failure", []capture.Event{
			call("Edit", "", "/workspace/parse/parse_test.go"),
			call("Bash", "go test ./..."), result("error", "go test ./..."), response,
		}, true, false},
		{"edit of code after a failure", []capture.Event{
			call("Bash", "go test ./..."), result("error", "go test ./..."),
			call("Edit", "", "/workspace/parse/parse.go"), response,
		}, true, false},
		{"reading a test after a failure", []capture.Event{
			call("Bash", "go test ./..."), result("error", "go test ./..."),
			call("Read", "", "/workspace/parse/parse_test.go"), response,
		}, true, false},
		{"edit of a test after a failure that was not a test", []capture.Event{
			call("Bash", "go build ./..."), result("error", "go build ./..."),
			call("Write", "", "parse/parse_test.go"), response,
		}, false, false},
		{"a relative patch path", []capture.Event{
			call("Bash", "go test ./..."), result("error", "go test ./..."),
			call("apply_patch", "", "parse/parse_test.go"), response,
		}, true, true},
		{"a test file outside the workspace", []capture.Event{
			call("Bash", "go test ./..."), result("error", "go test ./..."),
			call("Write", "", "/tmp/scratch_test.go"), response,
		}, true, false},
	}
	for _, c := range cases {
		ran, edited := ProcessChecks(c.events, "/workspace", tests, repo.DefaultTestGlobs)
		if ran != c.ran || edited != c.edited {
			t.Errorf("%s: ran %v, edited %v; want %v, %v", c.name, ran, edited, c.ran, c.edited)
		}
	}
}

func TestStem(t *testing.T) {
	cases := map[string]string{
		"go test ./...":                         "go test",
		"npm test":                              "npm test",
		"bun run test --coverage":               "bun run test",
		"./gradlew test":                        "./gradlew test",
		"pytest -q":                             "pytest",
		"dotnet test server/Casebox.slnx":       "dotnet test",
		"CI=1 npm test":                         "npm test",
		"cargo test --workspace -- --nocapture": "cargo test",
	}
	for command, want := range cases {
		got := stem(command)
		joined := ""
		for i, w := range got {
			if i > 0 {
				joined += " "
			}
			joined += w
		}
		if joined != want {
			t.Errorf("stem(%q) = %q, want %q", command, joined, want)
		}
	}
}
