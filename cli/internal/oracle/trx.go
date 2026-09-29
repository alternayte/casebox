package oracle

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
)

type trxRun struct {
	Results     []trxResult `xml:"Results>UnitTestResult"`
	Definitions []struct {
		ID     string `xml:"id,attr"`
		Method struct {
			ClassName string `xml:"className,attr"`
			Name      string `xml:"name,attr"`
		} `xml:"TestMethod"`
	} `xml:"TestDefinitions>UnitTest"`
	Summary struct {
		Outcome  string `xml:"outcome,attr"`
		RunInfos []struct {
			Outcome string `xml:"outcome,attr"`
			Text    string `xml:"Text"`
		} `xml:"RunInfos>RunInfo"`
	} `xml:"ResultSummary"`
}

type trxResult struct {
	TestID   string      `xml:"testId,attr"`
	TestName string      `xml:"testName,attr"`
	Duration string      `xml:"duration,attr"`
	Outcome  string      `xml:"outcome,attr"`
	StdOut   string      `xml:"Output>StdOut"`
	StdErr   string      `xml:"Output>StdErr"`
	Message  string      `xml:"Output>ErrorInfo>Message"`
	Stack    string      `xml:"Output>ErrorInfo>StackTrace"`
	Inner    []trxResult `xml:"InnerResults>UnitTestResult"`
}

// trxStatus maps a TRX outcome. An outcome that is not a finished pass, fail or skip is an error.
var trxStatus = map[string]Status{
	"Passed":              Passed,
	"PassedButRunAborted": Passed,
	"Completed":           Passed,
	"Warning":             Passed,
	"Failed":              Failed,
	"NotExecuted":         Skipped,
	"NotRunnable":         Skipped,
	"Inconclusive":        Skipped,
	"Timeout":             Error,
	"Error":               Error,
	"Aborted":             Error,
	"Disconnected":        Error,
	"InProgress":          Error,
	"Pending":             Error,
}

// parseTRX reads a .NET TRX file. Each UnitTestResult joins its UnitTestDefinition for the class
// name; the name is the result's testName, which keeps data-row arguments. A data-driven result
// is replaced by its rows. An aborted run or an error run info is a run error.
func parseTRX(data []byte) Run {
	var doc trxRun
	if err := xml.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &doc); err != nil {
		return Run{Error: "unreadable TRX file: " + err.Error()}
	}
	classes := make(map[string]string, len(doc.Definitions))
	for _, d := range doc.Definitions {
		// Older MSTest writes an assembly-qualified class name.
		class, _, _ := strings.Cut(d.Method.ClassName, ",")
		classes[d.ID] = strings.TrimSpace(class)
	}
	var run Run
	var add func(r trxResult, class string)
	add = func(r trxResult, class string) {
		if len(r.Inner) > 0 {
			for _, in := range r.Inner {
				add(in, class)
			}
			return
		}
		status, ok := trxStatus[r.Outcome]
		if !ok {
			status = Error
		}
		res := Result{ID: trxID(class, r.TestName), Status: status, Seconds: timeSpan(r.Duration)}
		if status != Passed {
			res.Output = capOutput(joinNonEmpty(r.Message, r.Stack, r.StdOut, r.StdErr))
		}
		run.Results = append(run.Results, res)
	}
	for _, r := range doc.Results {
		add(r, classes[r.TestID])
	}
	var errs []string
	switch doc.Summary.Outcome {
	case "Aborted", "Error", "Timeout":
		errs = append(errs, "the test run ended with outcome "+doc.Summary.Outcome)
	}
	for _, info := range doc.Summary.RunInfos {
		if info.Outcome == "Error" {
			errs = append(errs, strings.TrimSpace(info.Text))
		}
	}
	run.Error = capOutput(strings.Join(errs, "\n"))
	return run
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// trxID is class::name. xUnit and MSTest write testName with the class prefix or without it;
// the prefix is dropped either way. Without a definition, the class is the part of testName
// before the last dot outside the argument list.
func trxID(class, testName string) string {
	if class == "" {
		head := testName
		if i := strings.IndexByte(head, '('); i >= 0 {
			head = head[:i]
		}
		if i := strings.LastIndexByte(head, '.'); i >= 0 {
			return testName[:i] + "::" + testName[i+1:]
		}
		return "::" + testName
	}
	return class + "::" + strings.TrimPrefix(testName, class+".")
}

// timeSpan parses a .NET TimeSpan ([d.]hh:mm:ss[.fffffff]) into seconds; zero when malformed.
func timeSpan(s string) float64 {
	days := 0.0
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	if d, h, ok := strings.Cut(parts[0], "."); ok {
		n, err := strconv.ParseFloat(d, 64)
		if err != nil {
			return 0
		}
		days, parts[0] = n, h
	}
	h, err1 := strconv.ParseFloat(parts[0], 64)
	m, err2 := strconv.ParseFloat(parts[1], 64)
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0
	}
	return days*86400 + h*3600 + m*60 + sec
}

func joinNonEmpty(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n")
}
