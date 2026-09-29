package oracle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// goEvent is one line of go test -json (test2json).
type goEvent struct {
	Action      string
	Package     string
	Test        string
	Elapsed     float64
	Output      string
	ImportPath  string // build-output and build-fail events
	FailedBuild string // a package fail caused by a build failure
}

type goTest struct {
	id      string
	pkg     string
	status  Status // empty until the test reaches pass, fail or skip
	seconds float64
	output  strings.Builder
}

type goPackage struct {
	output      strings.Builder
	done        bool
	failed      bool
	failedBuild string
}

// parseGo reads go test -json output. A package that fails with no failing test (a build error,
// a crash in TestMain) or never finishes (a killed or truncated run) is a run error. A test that
// starts and never finishes (a timeout) or panics is an error. A test run more than once keeps
// its last result.
func parseGo(data []byte) Run {
	var (
		tests    []*goTest
		byID     = map[string]*goTest{}
		pkgs     = map[string]*goPackage{}
		pkgOrder []string
		build    = map[string]*strings.Builder{}
		stray    strings.Builder
		events   int
	)
	pkg := func(name string) *goPackage {
		p, ok := pkgs[name]
		if !ok {
			p = &goPackage{}
			pkgs[name] = p
			pkgOrder = append(pkgOrder, name)
		}
		return p
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var e goEvent
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if line[0] != '{' || json.Unmarshal(line, &e) != nil {
			// Not an event: go command output on the same stream, or a truncated last line.
			stray.Write(line)
			stray.WriteByte('\n')
			continue
		}
		events++
		switch e.Action {
		case "build-output":
			b, ok := build[e.ImportPath]
			if !ok {
				b = &strings.Builder{}
				build[e.ImportPath] = b
			}
			b.WriteString(e.Output)
			continue
		case "build-fail":
			continue
		}
		if e.Package == "" {
			continue
		}
		if e.Test == "" {
			p := pkg(e.Package)
			switch e.Action {
			case "output":
				p.output.WriteString(e.Output)
			case "pass", "skip":
				p.done = true
			case "fail":
				p.done, p.failed, p.failedBuild = true, true, e.FailedBuild
			}
			continue
		}
		pkg(e.Package)
		id := e.Package + "::" + e.Test
		t, ok := byID[id]
		if !ok {
			t = &goTest{id: id, pkg: e.Package}
			byID[id] = t
			tests = append(tests, t)
		}
		switch e.Action {
		case "run":
			if t.status != "" {
				// A repeated run (-count): the new run replaces the old one.
				t.status, t.seconds = "", 0
				t.output.Reset()
			}
		case "output":
			t.output.WriteString(e.Output)
		case "pass":
			t.status, t.seconds = Passed, e.Elapsed
		case "fail":
			t.status, t.seconds = Failed, e.Elapsed
		case "skip":
			t.status, t.seconds = Skipped, e.Elapsed
		}
	}
	if err := sc.Err(); err != nil {
		return Run{Error: "reading go test output: " + err.Error()}
	}
	if events == 0 {
		msg := "no go test -json events in the results file"
		if s := strings.TrimSpace(stray.String()); s != "" {
			msg += ":\n" + s
		}
		return Run{Error: capOutput(msg)}
	}

	var run Run
	explained := map[string]bool{}
	for _, t := range tests {
		out := t.output.String()
		switch {
		case t.status == "":
			t.status = Error
		case t.status == Failed && panicked(out):
			t.status = Error
		}
		if t.status == Failed || t.status == Error {
			explained[t.pkg] = true
		}
		r := Result{ID: t.id, Status: t.status, Seconds: t.seconds}
		if t.status != Passed {
			r.Output = capOutput(out)
		}
		run.Results = append(run.Results, r)
	}
	var errs []string
	for _, name := range pkgOrder {
		p := pkgs[name]
		switch {
		case !p.done:
			errs = append(errs, fmt.Sprintf("%s: the run stopped before the package finished\n%s", name, p.output.String()))
		case p.failed && p.failedBuild != "":
			msg := name + ": build failed\n"
			if b := build[p.failedBuild]; b != nil {
				msg += b.String()
			}
			errs = append(errs, msg+p.output.String())
		case p.failed && !explained[name]:
			errs = append(errs, fmt.Sprintf("%s: failed outside any test\n%s", name, p.output.String()))
		}
	}
	run.Error = capOutput(strings.TrimSpace(strings.Join(errs, "\n")))
	return run
}

// panicked reports whether test output holds a runtime panic, which the testing package prints
// at the start of a line; t.Log output is indented.
func panicked(out string) bool {
	return strings.HasPrefix(out, "panic: ") || strings.Contains(out, "\npanic: ")
}
