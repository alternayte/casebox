package oracle

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// junitSuite reads both <testsuites> and <testsuite> roots: the first holds suites, the second
// test cases, and a suite may nest further suites.
type junitSuite struct {
	Name      string       `xml:"name,attr"`
	Errors    string       `xml:"errors,attr"`
	Cases     []junitCase  `xml:"testcase"`
	Suites    []junitSuite `xml:"testsuite"`
	SystemOut string       `xml:"system-out"`
	SystemErr string       `xml:"system-err"`
}

type junitCase struct {
	ClassName string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitProblem `xml:"failure"`
	Error     *junitProblem `xml:"error"`
	Skipped   *junitProblem `xml:"skipped"`
	SystemOut string        `xml:"system-out"`
	SystemErr string        `xml:"system-err"`
}

type junitProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// parseJUnit reads JUnit XML (pytest, jest-junit, Maven Surefire). A suite that reports errors
// and holds no test cases failed to run, which is a run error.
func parseJUnit(data []byte) Run {
	var root junitSuite
	if err := xml.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &root); err != nil {
		return Run{Error: "unreadable JUnit file: " + err.Error()}
	}
	var run Run
	var errs []string
	// walk reports whether it found a suite that failed to run, so an enclosing suite whose
	// counts include it is not reported again.
	var walk func(s junitSuite) bool
	walk = func(s junitSuite) bool {
		for _, c := range s.Cases {
			run.Results = append(run.Results, junitResult(c))
		}
		reported := false
		for _, child := range s.Suites {
			reported = walk(child) || reported
		}
		if n, _ := strconv.Atoi(strings.TrimSpace(s.Errors)); n > 0 && !reported && countCases(s) == 0 {
			errs = append(errs, strings.TrimSpace(fmt.Sprintf("suite %q failed to run\n%s", s.Name, joinNonEmpty(s.SystemErr, s.SystemOut))))
			reported = true
		}
		return reported
	}
	walk(root)
	run.Error = capOutput(strings.Join(errs, "\n"))
	return run
}

func junitResult(c junitCase) Result {
	r := Result{ID: c.ClassName + "::" + c.Name, Status: Passed, Seconds: junitTime(c.Time)}
	var p *junitProblem
	switch {
	case c.Error != nil:
		r.Status, p = Error, c.Error
	case c.Failure != nil:
		r.Status, p = Failed, c.Failure
	case c.Skipped != nil:
		r.Status, p = Skipped, c.Skipped
	}
	if p != nil {
		text := strings.TrimSpace(p.Text)
		msg := strings.TrimSpace(p.Message)
		if msg != "" && strings.Contains(text, msg) {
			msg = ""
		}
		r.Output = capOutput(joinNonEmpty(msg, text, c.SystemOut, c.SystemErr))
	}
	return r
}

func countCases(s junitSuite) int {
	n := len(s.Cases)
	for _, child := range s.Suites {
		n += countCases(child)
	}
	return n
}

// junitTime parses a time attribute in seconds; some writers group thousands with commas.
func junitTime(s string) float64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", ""), 64)
	if err != nil {
		return 0
	}
	return f
}
