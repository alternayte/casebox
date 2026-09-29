package evaluate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/alternayte/casebox/cli/internal/analysis"
	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/cases"
	"github.com/alternayte/casebox/cli/internal/oracle"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/runner"
	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// VerifyPayload is the payload of a verify job, as the server writes it after a run. Instruction
// is read when the server sends it; without it the judge sees the case's source task.
type VerifyPayload struct {
	EvaluationID string           `json:"evaluationId"`
	RunID        string           `json:"runId"`
	CaseID       string           `json:"caseId"`
	Recipe       repo.Recipe      `json:"recipe"`
	Repos        []cases.CaseRepo `json:"repos"`
	Oracle       string           `json:"oracle"`
	Diff         *string          `json:"diff"`
	Trace        *string          `json:"trace"`
	Instruction  string           `json:"instruction,omitempty"`
	// The person-approved steering assertions and judge question of the case; they win over the
	// oracle blob's, which validation writes before anyone approves them.
	Assertions []cases.Assertion     `json:"assertions,omitempty"`
	Judge      []cases.JudgeQuestion `json:"judge,omitempty"`
}

// VerifyAnswer is the answer to a verify job (the server's VerifyAnswer record).
type VerifyAnswer struct {
	RunID      string            `json:"runId"`
	Applied    bool              `json:"applied"`
	Tests      *TestCounts       `json:"tests"`
	Failed     []string          `json:"failed"`
	Assertions []AssertionResult `json:"assertions"`
	Judge      *JudgeAnswer      `json:"judge"`
	Passed     bool              `json:"passed"`
}

// TestCount is how many of a list of tests passed.
type TestCount struct {
	Passed int `json:"passed"`
	Total  int `json:"total"`
}

// TestCounts are the fail-to-pass and pass-to-pass counts of a verification.
type TestCounts struct {
	FailToPass TestCount `json:"failToPass"`
	PassToPass TestCount `json:"passToPass"`
}

// AssertionResult is one steering assertion's outcome.
type AssertionResult struct {
	Kind   string `json:"kind"`
	Passed bool   `json:"passed"`
}

// JudgeAnswer is the judge model's answer, which is medium evidence.
type JudgeAnswer struct {
	Answer   string `json:"answer"`
	Evidence string `json:"evidence"`
}

// Verify runs a verify job: the agent's diff on a pristine base in a sandbox with no model key.
func (j Jobs) Verify(ctx context.Context, job worker.Job) (any, error) {
	var p VerifyPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.RunID == "" || p.CaseID == "" || len(p.Repos) == 0 {
		return nil, worker.Permanent{Err: errors.New("the job names no run, case or repository")}
	}
	return verify(ctx, j.deps(), p)
}

func verify(ctx context.Context, d deps, pl VerifyPayload) (VerifyAnswer, error) {
	if err := pl.Recipe.Validate(); err != nil {
		return VerifyAnswer{}, worker.Permanent{Err: err}
	}
	o, err := readOracle(ctx, d, pl.Oracle)
	if err != nil {
		return VerifyAnswer{}, err
	}
	var diff, testPatch []byte
	if pl.Diff != nil && *pl.Diff != "" {
		if diff, err = d.get(ctx, *pl.Diff); err != nil {
			return VerifyAnswer{}, fmt.Errorf("read the agent's diff: %w", err)
		}
	}
	if o.TestPatch != "" {
		if testPatch, err = d.get(ctx, o.TestPatch); err != nil {
			return VerifyAnswer{}, fmt.Errorf("read the test patch: %w", err)
		}
	}
	var trace Trace
	if pl.Trace != nil && *pl.Trace != "" {
		raw, err := d.get(ctx, *pl.Trace)
		if err != nil {
			return VerifyAnswer{}, fmt.Errorf("read the trace: %w", err)
		}
		if err := json.Unmarshal(raw, &trace); err != nil {
			return VerifyAnswer{}, worker.Permanent{Err: fmt.Errorf("the trace %s is not JSON: %w", short(*pl.Trace), err)}
		}
	}

	sealed, err := openSealed(ctx, d, pl.Repos)
	if err != nil {
		return VerifyAnswer{}, err
	}
	tree, files, err := baseTree(ctx, sealed)
	if err != nil {
		return VerifyAnswer{}, err
	}
	defer os.Remove(tree.Name())
	defer tree.Close()
	spec, err := envSpec(ctx, pl.Recipe, sealed, files)
	if err != nil {
		return VerifyAnswer{}, err
	}

	// No agent runs here, so the verifier reaches the registries directly, as validation's does.
	egress := cases.Registries(pl.Recipe)
	if err := runner.CheckEgress(egress); err != nil {
		return VerifyAnswer{}, worker.Permanent{Err: err}
	}
	res, err := runner.Verify(ctx, d.provider, spec, tree, diff, testPatch, o.TestFiles, pl.Recipe.Test, egress)
	if err != nil && len(egress) > 0 && errors.Is(err, sandbox.ErrUnsupported) {
		// A provider without an allow-list runs the tests with no network, as validation does.
		if _, serr := tree.Seek(0, io.SeekStart); serr != nil {
			return VerifyAnswer{}, serr
		}
		res, err = runner.Verify(ctx, d.provider, spec, tree, diff, testPatch, o.TestFiles, pl.Recipe.Test, nil)
	}
	if err != nil {
		return VerifyAnswer{}, err
	}

	counts, failed, err := Count(o, res)
	if err != nil {
		return VerifyAnswer{}, worker.Permanent{Err: err}
	}
	assertions, questions := o.Assertions, o.Judge
	if pl.Assertions != nil {
		assertions = pl.Assertions
	}
	if pl.Judge != nil {
		questions = pl.Judge
	}
	answer := VerifyAnswer{
		RunID:      pl.RunID,
		Applied:    res.Applied,
		Tests:      &counts,
		Failed:     failed,
		Assertions: CheckAssertions(assertions, diff, trace.Events),
	}
	if len(questions) > 0 && d.model != nil {
		task := pl.Instruction
		if task == "" {
			if task, err = sourceTask(ctx, d, pl.CaseID); err != nil {
				return VerifyAnswer{}, fmt.Errorf("read the case's task for the judge: %w", err)
			}
		}
		if answer.Judge, err = judge(ctx, d.model, questions, task, diff); err != nil {
			return VerifyAnswer{}, err
		}
	}
	answer.Passed = Passed(answer)
	return answer, nil
}

// Passed is the verdict of the spec: the diff applied, every fail-to-pass and pass-to-pass test
// and every assertion passed, and a judge, when there is one, did not answer no. The server
// recomputes it.
func Passed(a VerifyAnswer) bool {
	if !a.Applied || a.Tests == nil {
		return false
	}
	t := a.Tests
	if t.FailToPass.Passed != t.FailToPass.Total || t.PassToPass.Passed != t.PassToPass.Total {
		return false
	}
	for _, as := range a.Assertions {
		if !as.Passed {
			return false
		}
	}
	return a.Judge == nil || strings.EqualFold(a.Judge.Answer, "yes")
}

// Count reads the verifier's result files through the oracle and counts the oracle's fail-to-pass
// and pass-to-pass tests that passed; a test with no result counts as failed. A test command that
// writes no results file passed validation by its exit code, so it counts as one more pass-to-pass
// test, named exit:<command>. Failed lists the tests that did not pass. When the diff did not
// apply, nothing ran and every test failed.
func Count(o cases.Oracle, res runner.Result) (TestCounts, []string, error) {
	var run oracle.Run
	type exit struct {
		command string
		passed  bool
	}
	var exits []exit
	for _, c := range res.Commands {
		if c.Results == "" {
			exits = append(exits, exit{command: c.Command, passed: c.ExitCode == 0 && !c.TimedOut})
			continue
		}
		r, err := oracle.ParseFiles(c.Results, resultFiles(c.Results, c.Files))
		if err != nil {
			return TestCounts{}, nil, err
		}
		run.Results = append(run.Results, r.Results...)
	}
	if !res.Applied {
		// Nothing ran; the exit-only commands are counted as failed below.
		exits = exits[:0]
		for _, c := range o.Commands {
			if c.Results == "" {
				exits = append(exits, exit{command: c.Command})
			}
		}
	}
	byID := run.ByID()
	failed := []string{}
	countOf := func(ids []string) TestCount {
		n := TestCount{Total: len(ids)}
		for _, id := range ids {
			if byID[id].Status == oracle.Passed {
				n.Passed++
			} else {
				failed = append(failed, id)
			}
		}
		return n
	}
	counts := TestCounts{FailToPass: countOf(o.Tests.FailToPass), PassToPass: countOf(o.Tests.PassToPass)}
	for _, e := range exits {
		counts.PassToPass.Total++
		if e.passed {
			counts.PassToPass.Passed++
		} else {
			failed = append(failed, "exit:"+e.command)
		}
	}
	return counts, failed, nil
}

// resultFiles keeps the files of a results directory that the format's parser reads, as
// validation does: .trx for TRX, .xml for JUnit, every file for go test -json.
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

// Assertion kinds.
const (
	ForbiddenFile     = "forbidden_file"
	CommandBeforeDone = "command_before_done"
	DiffMustMatch     = "diff_must_match"
	DiffMustNotMatch  = "diff_must_not_match"
)

// CheckAssertions checks the case's steering assertions against the agent's diff and trace:
//
//   - forbidden_file passes when the diff touches no path the glob matches;
//   - command_before_done passes when a tool call whose command the pattern matches comes before
//     the agent's last response;
//   - diff_must_match and diff_must_not_match match the pattern against the lines the diff adds.
//
// A pattern that does not compile, or an unknown kind, fails.
func CheckAssertions(assertions []cases.Assertion, diff []byte, events []capture.Event) []AssertionResult {
	out := []AssertionResult{}
	for _, a := range assertions {
		out = append(out, AssertionResult{Kind: a.Kind, Passed: checkAssertion(a, diff, events)})
	}
	return out
}

func checkAssertion(a cases.Assertion, diff []byte, events []capture.Event) bool {
	switch a.Kind {
	case ForbiddenFile:
		if a.Path == "" {
			return false
		}
		for _, f := range diffPaths(diff) {
			if repo.MatchGlob(a.Path, f) {
				return false
			}
		}
		return true
	case CommandBeforeDone:
		re, err := regexp.Compile(a.Pattern)
		if err != nil || a.Pattern == "" {
			return false
		}
		last := -1
		for i, e := range events {
			if e.Kind == capture.KindResponse {
				last = i
			}
		}
		for i := 0; i < last; i++ {
			if e := events[i]; e.Kind == capture.KindToolCall && e.Attrs["command"] != "" && re.MatchString(e.Attrs["command"]) {
				return true
			}
		}
		return false
	case DiffMustMatch, DiffMustNotMatch:
		re, err := regexp.Compile(a.Pattern)
		if err != nil || a.Pattern == "" {
			return false
		}
		return re.MatchString(addedLines(diff)) == (a.Kind == DiffMustMatch)
	default:
		return false
	}
}

// diffPaths are the paths a git diff made with --no-renames touches.
func diffPaths(diff []byte) []string {
	parts, err := cases.SplitPatch(diff)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Path)
	}
	return out
}

// addedLines are the lines a git diff adds, without their "+", one per line.
func addedLines(diff []byte) string {
	var b strings.Builder
	inHunk := false
	for _, line := range bytes.Split(diff, []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte("diff --git ")):
			inHunk = false
		case bytes.HasPrefix(line, []byte("@@")):
			inHunk = true
		case inHunk && len(line) > 0 && line[0] == '+':
			b.Write(bytes.TrimSuffix(line[1:], []byte("\r")))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// sourceTask is the case's task as its source states it: the work item's title and description,
// the pull request title, or the steering session's prompts.
func sourceTask(ctx context.Context, d deps, caseID string) (string, error) {
	src, err := d.source(ctx, caseID)
	if err != nil {
		return "", err
	}
	var parts []string
	switch {
	case src.WorkItem != nil:
		parts = append(parts, src.WorkItem.Title)
		if src.WorkItem.Description != nil {
			parts = append(parts, *src.WorkItem.Description)
		}
	case src.PullTitle != nil:
		parts = append(parts, *src.PullTitle)
	case src.Steering != nil:
		parts = append(parts, src.Steering.Prompts...)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n")), nil
}

// maxJudgeDiff caps the diff the judge reads.
const maxJudgeDiff = 200_000

const judgeSystem = `You judge one change a coding agent made for a task. You get the task and the agent's diff. Answer the question about the diff with yes or no, from what the diff shows. Answer no when the diff does not show enough to answer yes.`

var judgeSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"answer": map[string]any{"type": "string", "enum": []string{"yes", "no"}},
		"reason": map[string]any{"type": "string", "description": "One sentence that names what in the diff decides the answer."},
	},
	"required": []string{"answer", "reason"},
}

// judge asks each question with the task and the agent's diff, never the held-out tests. The
// answer is yes only when every question is answered yes.
func judge(ctx context.Context, model *analysis.Client, questions []cases.JudgeQuestion, task string, diff []byte) (*JudgeAnswer, error) {
	answer := "yes"
	asked := 0
	for _, q := range questions {
		question := strings.TrimSpace(q.Question)
		if question == "" {
			continue
		}
		asked++
		user := "## Task\n\n" + task + "\n\n## The agent's diff\n\n```diff\n" + clip(string(diff), maxJudgeDiff) + "\n```\n\n## Question\n\n" + question
		reply, err := model.Complete(ctx, analysis.Request{System: judgeSystem, User: user, Tool: "judgement", Schema: judgeSchema, MaxTokens: 512})
		if err != nil {
			return nil, fmt.Errorf("ask the judge: %w", err)
		}
		var got struct {
			Answer string `json:"answer"`
		}
		if err := json.Unmarshal(reply.JSON, &got); err != nil {
			return nil, fmt.Errorf("the judge's answer is not the JSON asked for: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(got.Answer)) {
		case "yes":
		case "no":
			answer = "no"
		default:
			return nil, fmt.Errorf("the judge answered %q, not yes or no", got.Answer)
		}
	}
	if asked == 0 {
		return nil, nil
	}
	return &JudgeAnswer{Answer: answer, Evidence: "medium"}, nil
}
