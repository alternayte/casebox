package sources_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/sources/claudecode"
	"github.com/alternayte/casebox/cli/internal/sources/codex"
	"github.com/alternayte/casebox/cli/internal/sources/cursor"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// Each fixture is a scrubbed real transcript (tools/scrubfixture). Its golden file is the parser's
// canonical output; a parser change that alters it shows up in review.
func TestGoldenFiles(t *testing.T) {
	parsers := map[string]func(*os.File) (any, error){
		"claudecode": func(f *os.File) (any, error) { return claudecode.Parse(f) },
		"codex":      func(f *os.File) (any, error) { return codex.Parse(f) },
		"cursor": func(f *os.File) (any, error) {
			session, events, err := cursor.Parse(f, f.Name(), time.Date(2026, 9, 29, 6, 0, 0, 0, time.UTC))
			return struct {
				Session any
				Events  any
			}{session, events}, err
		},
	}
	for agent, parse := range parsers {
		fixtures, _ := filepath.Glob(filepath.Join("testdata", agent, "*.jsonl"))
		if len(fixtures) == 0 {
			t.Fatalf("no fixtures for %s", agent)
		}
		for _, fixture := range fixtures {
			t.Run(agent+"/"+filepath.Base(fixture), func(t *testing.T) {
				f, err := os.Open(fixture)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				res, err := parse(f)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := json.MarshalIndent(res, "", "  ")
				got = append(got, '\n')
				golden := strings.TrimSuffix(fixture, ".jsonl") + ".golden.json"
				if *update {
					if err := os.WriteFile(golden, got, 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("%v; run go test ./internal/sources -update", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("the parser's output differs from %s; if the change is intended, run go test ./internal/sources -update and review the diff", golden)
				}
			})
		}
	}
}
