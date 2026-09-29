package gitai

import (
	"reflect"
	"testing"
)

const note = `src/api/handler.go
  s_3f9a0c1d2e4b57::t_8c1e2a9b0d7f64 1-24,30-41
  h_a1b2c3d4e5f607 25-29
  s_3f9a0c1d2e4b57::t_02d4f6a8c0e1b3 42,55,50-54,280,281-284
  human 60-70
"docs/How To.md"
  0123456789abcdef 3-12
  ffffffffffffffff 20
---
{
  "schema_version": "authorship/3.0.0",
  "base_commit_sha": "9e1f0c3b7a2d4e5f60718293a4b5c6d7e8f90123",
  "prompts": {"0123456789abcdef": {"agent_id": {"tool": "cursor", "id": "x", "model": "gpt-5"}, "human_author": "Dana Lee <dana@example.com>"}},
  "humans": {"h_a1b2c3d4e5f607": {"author": "Dana Lee <dana@example.com>"}},
  "sessions": {"s_3f9a0c1d2e4b57": {"agent_id": {"tool": "claude", "id": "y", "model": "claude-sonnet-5"}, "human_author": null}}
}`

// Real notes have unsorted and adjacent ranges, junk keys and prompts with no record.
func TestANoteGivesOnlyAgentLinesMergedAndSorted(t *testing.T) {
	got, err := Parse(note)
	if err != nil {
		t.Fatal(err)
	}
	want := []Attribution{
		{Path: "src/api/handler.go", Agent: "claude", Model: "claude-sonnet-5", Ranges: "1-24,30-42,50-55,280-284"},
		{Path: "docs/How To.md", Agent: "cursor", Model: "gpt-5", Ranges: "3-12"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}
