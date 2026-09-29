// Package oracle reads test results per test: go test -json, .NET TRX and JUnit XML. A run that
// crashed (a build error, a panic outside a test, a missing or truncated results file) carries an
// error and is never a pass.
package oracle

import (
	"fmt"
	"sort"
	"strings"
)

// Status is the outcome of one test.
type Status string

// Test outcomes.
const (
	Passed  Status = "passed"
	Failed  Status = "failed"
	Skipped Status = "skipped"
	Error   Status = "error"
)

// Result formats.
const (
	GoTestJSON = "go-test-json"
	TRX        = "trx"
	JUnit      = "junit"
)

// maxOutput caps the output kept per test and per run error.
const maxOutput = 8 << 10

// Result is one test's outcome. ID is <package or class>::<name>.
type Result struct {
	ID      string  `json:"id"`
	Status  Status  `json:"status"`
	Seconds float64 `json:"seconds"`
	Output  string  `json:"output,omitempty"`
}

// Run is the parsed results of one test run. Error is set when the run crashed or produced no
// parseable results.
type Run struct {
	Results []Result `json:"results"`
	Error   string   `json:"error,omitempty"`
}

// Parse reads one results file in the given format. The error is for an unknown format only; a
// broken file is a crashed run, reported in Run.Error.
func Parse(format string, data []byte) (Run, error) {
	var run Run
	switch format {
	case GoTestJSON:
		run = parseGo(data)
	case TRX:
		run = parseTRX(data)
	case JUnit:
		run = parseJUnit(data)
	default:
		return Run{}, fmt.Errorf("unknown results format %q (want %s, %s or %s)", format, GoTestJSON, TRX, JUnit)
	}
	if run.Error == "" && len(run.Results) == 0 {
		run.Error = "no test results"
	}
	return run, nil
}

// ParseFiles reads several results files of one format as one run, in file name order; a later
// result for the same test wins. An empty file is a crashed run.
func ParseFiles(format string, files map[string][]byte) (Run, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var out Run
	var errs []string
	index := map[string]int{}
	for _, name := range names {
		run, err := Parse(format, files[name])
		if err != nil {
			return Run{}, err
		}
		if run.Error != "" {
			errs = append(errs, name+": "+run.Error)
		}
		for _, r := range run.Results {
			if i, ok := index[r.ID]; ok {
				out.Results[i] = r
				continue
			}
			index[r.ID] = len(out.Results)
			out.Results = append(out.Results, r)
		}
	}
	if len(names) == 0 {
		errs = append(errs, "no results files")
	}
	out.Error = capOutput(strings.Join(errs, "\n"))
	return out, nil
}

// ByID indexes the results by test ID.
func (r Run) ByID() map[string]Result {
	m := make(map[string]Result, len(r.Results))
	for _, res := range r.Results {
		m[res.ID] = res
	}
	return m
}

// FailToPass returns the tests that pass in every merged run and pass in no base run: they
// failed, errored or did not exist at the base. A merged run that crashed passes nothing. Sorted.
func FailToPass(base, merged []Run) []string {
	if len(merged) == 0 {
		return nil
	}
	baseIDs := make([]map[string]Result, len(base))
	for i, b := range base {
		baseIDs[i] = b.ByID()
	}
	var ids []string
	for _, id := range passingIn(merged) {
		passedAtBase := false
		for _, b := range baseIDs {
			if b[id].Status == Passed {
				passedAtBase = true
				break
			}
		}
		if !passedAtBase {
			ids = append(ids, id)
		}
	}
	return ids
}

// PassToPass returns the in-scope tests that pass at the base and in every merged run, sorted,
// at most limit of them (no limit when limit <= 0). A nil inScope keeps every test.
func PassToPass(base Run, merged []Run, inScope func(id string) bool, limit int) []string {
	if len(merged) == 0 {
		return nil
	}
	atBase := base.ByID()
	var ids []string
	for _, id := range passingIn(merged) {
		if atBase[id].Status != Passed || (inScope != nil && !inScope(id)) {
			continue
		}
		ids = append(ids, id)
		if limit > 0 && len(ids) == limit {
			break
		}
	}
	return ids
}

// Flaky returns the ids that do not pass in every run, in the order given. A crashed run
// passes nothing.
func Flaky(merged []Run, ids []string) []string {
	byRun := make([]map[string]Result, len(merged))
	for i, m := range merged {
		byRun[i] = m.ByID()
	}
	var out []string
	for _, id := range ids {
		for i, m := range merged {
			if m.Error != "" || byRun[i][id].Status != Passed {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// passingIn returns the tests that pass in every run, sorted. A crashed run passes nothing.
func passingIn(runs []Run) []string {
	count := map[string]int{}
	for _, run := range runs {
		if run.Error != "" {
			return nil
		}
		for id, r := range run.ByID() {
			if r.Status == Passed {
				count[id]++
			}
		}
	}
	var ids []string
	for id, n := range count {
		if n == len(runs) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// capOutput keeps the head and the tail of s within maxOutput bytes.
func capOutput(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	const marker = "\n… (output cut) …\n"
	half := (maxOutput - len(marker)) / 2
	// Drop a UTF-8 sequence split at either cut.
	return strings.ToValidUTF8(s[:half], "") + marker + strings.ToValidUTF8(s[len(s)-half:], "")
}
