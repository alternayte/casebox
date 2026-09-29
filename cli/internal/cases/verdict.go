package cases

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/oracle"
	"github.com/alternayte/casebox/cli/internal/runner"
)

// Validation failure reasons, as the server reads them.
const (
	ReasonBuild                  = "build"
	ReasonNoFailToPass           = "no_fail_to_pass"
	ReasonFailToPassPassedAtBase = "fail_to_pass_passed_at_base"
	ReasonFlaky                  = "flaky"
	ReasonTooSlow                = "too_slow"
	ReasonFixDoesNotFail         = "fix_does_not_fail"
	ReasonApplyFailed            = "apply_failed"
)

// MaxRunTime is the most the slowest of the three merged runs may take.
const MaxRunTime = 10 * time.Minute

// MaxPassToPass caps the pass-to-pass tests of a case.
const MaxPassToPass = 200

// runOutcome is one verifier run read through the oracle: the per-test results of every command
// that writes results, and the exit of every command that does not.
type runOutcome struct {
	Applied     bool
	FailedPatch string
	ApplyOutput string
	Run         oracle.Run
	Exits       []exitResult
	Duration    time.Duration
	TimedOut    bool
	Output      string
}

// exitResult is a command without results: it counts pass or fail by its exit code only, and
// cannot give fail-to-pass tests.
type exitResult struct {
	Command  string
	ExitCode int
}

// outcome parses a runner result. A command's results files are read in its format; a command
// that crashed or left no results makes the run an error.
func outcome(res runner.Result) (runOutcome, error) {
	o := runOutcome{Applied: res.Applied, FailedPatch: res.FailedPatch, ApplyOutput: res.ApplyOutput}
	var errs, outputs []string
	for _, c := range res.Commands {
		o.Duration += c.Duration
		o.TimedOut = o.TimedOut || c.TimedOut
		if c.ExitCode != 0 || c.TimedOut {
			outputs = append(outputs, fmt.Sprintf("$ %s (exit %d)\n%s", c.Command, c.ExitCode, c.Output))
		}
		if c.Results == "" {
			code := c.ExitCode
			if c.TimedOut && code == 0 {
				code = -1
			}
			o.Exits = append(o.Exits, exitResult{Command: c.Command, ExitCode: code})
			continue
		}
		run, err := oracle.ParseFiles(c.Results, resultFiles(c.Results, c.Files))
		if err != nil {
			return runOutcome{}, err
		}
		if run.Error != "" {
			errs = append(errs, c.Command+": "+run.Error)
		}
		o.Run.Results = append(o.Run.Results, run.Results...)
	}
	o.Run.Error = strings.Join(errs, "\n")
	o.Output = strings.Join(outputs, "\n")
	return o, nil
}

// resultFiles keeps the files of a results directory that the format's parser reads: .trx for
// TRX, .xml for JUnit (surefire and Gradle reports hold .txt files next to them), and every file
// for go test -json.
func resultFiles(format string, files map[string][]byte) map[string][]byte {
	ext := map[string]string{oracle.TRX: ".trx", oracle.JUnit: ".xml"}[format]
	if ext == "" {
		return files
	}
	out := map[string][]byte{}
	for name, body := range files {
		if strings.EqualFold(path.Ext(name), ext) {
			out[name] = body
		}
	}
	return out
}

// verdict is what the base run and the merged runs decide: a failure reason, or the oracle's
// tests.
type verdict struct {
	Reason     string
	Detail     string
	FailToPass []string
	PassToPass []string
	Slowest    time.Duration
}

// decide applies the validation rules to the base run (the base with the test patch) and the
// merged runs (the source and test patches, three times). Every fail-to-pass and pass-to-pass
// test must pass in every merged run; the slowest merged run must finish within MaxRunTime; a
// capability or regression case needs at least one fail-to-pass test. inScope limits
// pass-to-pass to the touched packages.
func decide(kind string, base runOutcome, merged []runOutcome, inScope func(id string) bool) verdict {
	var v verdict
	for _, m := range merged {
		v.Slowest = max(v.Slowest, m.Duration)
	}
	for i, m := range merged {
		if m.TimedOut {
			v.Reason, v.Detail = ReasonTooSlow, fmt.Sprintf("merged run %d hit a test command's timeout\n%s", i+1, m.Output)
			return v
		}
	}
	if v.Slowest >= MaxRunTime {
		v.Reason, v.Detail = ReasonTooSlow, fmt.Sprintf("the slowest merged run took %s; the limit is %s", v.Slowest.Round(time.Second), MaxRunTime)
		return v
	}

	crashed := 0
	var firstCrash string
	for i, m := range merged {
		if m.Run.Error != "" {
			crashed++
			if firstCrash == "" {
				firstCrash = fmt.Sprintf("merged run %d: %s", i+1, m.Run.Error)
			}
		}
	}
	if crashed == len(merged) && crashed > 0 {
		v.Reason, v.Detail = ReasonBuild, "the tests do not build or run with the merged change: "+firstCrash
		return v
	}
	if crashed > 0 {
		v.Reason, v.Detail = ReasonFlaky, fmt.Sprintf("%d of %d merged runs crashed: %s", crashed, len(merged), firstCrash)
		return v
	}
	if len(merged) > 0 {
		for ci, e := range merged[0].Exits {
			failed := 0
			for _, m := range merged {
				if ci < len(m.Exits) && m.Exits[ci].ExitCode != 0 {
					failed++
				}
			}
			switch {
			case failed == len(merged):
				v.Reason, v.Detail = ReasonBuild, fmt.Sprintf("%q fails with the merged change\n%s", e.Command, merged[0].Output)
				return v
			case failed > 0:
				v.Reason, v.Detail = ReasonFlaky, fmt.Sprintf("%q failed in %d of %d merged runs", e.Command, failed, len(merged))
				return v
			}
		}
	}

	// Candidates: tests that pass in at least one merged run and either did not pass at the base
	// (fail-to-pass) or passed there and are in scope (pass-to-pass). Each must pass 3 of 3.
	atBase := base.Run.ByID()
	seen := map[string]bool{}
	var candidates []string
	for _, m := range merged {
		for _, r := range m.Run.Results {
			if r.Status != oracle.Passed || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			if atBase[r.ID].Status != oracle.Passed || inScope == nil || inScope(r.ID) {
				candidates = append(candidates, r.ID)
			}
		}
	}
	sort.Strings(candidates)
	runs := make([]oracle.Run, len(merged))
	for i, m := range merged {
		runs[i] = m.Run
	}
	if flaky := oracle.Flaky(runs, candidates); len(flaky) > 0 {
		v.Reason, v.Detail = ReasonFlaky, fmt.Sprintf("%d tests do not pass in every merged run: %s", len(flaky), list(flaky, 20))
		return v
	}

	v.FailToPass = oracle.FailToPass([]oracle.Run{base.Run}, runs)
	v.PassToPass = oracle.PassToPass(base.Run, runs, inScope, MaxPassToPass)
	if len(v.FailToPass) == 0 && kind != Steering {
		switch {
		case len(merged) > 0 && len(merged[0].Run.Results) == 0 && len(merged[0].Exits) > 0:
			v.Reason, v.Detail = ReasonNoFailToPass, "no test command writes per-test results, so no test can be fail-to-pass"
		case len(v.PassToPass) > 0:
			v.Reason, v.Detail = ReasonFailToPassPassedAtBase, "every test that passes with the merged change in the touched packages already passes at the base with the test patch"
		default:
			v.Reason, v.Detail = ReasonNoFailToPass, "no test fails at the base and passes with the merged change"
		}
	}
	return v
}

func list(ids []string, n int) string {
	if len(ids) <= n {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:n], ", ") + fmt.Sprintf(" and %d more", len(ids)-n)
}

// rootSegments are directory names that hold packages without naming them, so they do not have
// to appear in a test ID: src/test/java/com/acme/store is the package com.acme.store.
var rootSegments = map[string]bool{
	"src": true, "lib": true, "main": true, "test": true, "tests": true, "java": true, "kotlin": true, "scala": true,
	"source": true, "sources": true, "app": true, "packages": true,
}

// scopeOf returns whether a test ID (<package or class>::<name>) is in one of the touched
// directories. A Go ID names its package's import path, which ends in the package's directory:
// the last segments must agree. A .NET or JUnit ID names a dotted class: the directory's
// significant segments must appear in it, in order, so a .NET test class matches its project's
// folder and a JUnit class its Java package (src/test/java/com/acme/store is com.acme.store). A
// directory with no significant segment (the root, src, tests) touches every test.
func scopeOf(dirs []string) func(id string) bool {
	var last []string
	var wanted [][]string
	for _, d := range dirs {
		full := segments(d)
		segs := segments(strings.ReplaceAll(d, ".", "/"))
		for len(segs) > 0 && rootSegments[segs[0]] {
			segs = segs[1:]
		}
		if len(full) == 0 || len(segs) == 0 {
			return nil
		}
		last = append(last, full[len(full)-1])
		wanted = append(wanted, segs)
	}
	return func(id string) bool {
		pkg, _, _ := strings.Cut(id, "::")
		if strings.Contains(pkg, "/") {
			have := segments(pkg)
			for _, l := range last {
				if len(have) > 0 && have[len(have)-1] == l {
					return true
				}
			}
			return false
		}
		have := segments(strings.NewReplacer(".", "/", "\\", "/", ":", "/").Replace(pkg))
		for _, w := range wanted {
			if contains(have, w) {
				return true
			}
		}
		return false
	}
}

func segments(p string) []string {
	var out []string
	for _, s := range strings.Split(strings.ToLower(p), "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func contains(have, want []string) bool {
	for i := 0; i+len(want) <= len(have); i++ {
		match := true
		for j := range want {
			if have[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
