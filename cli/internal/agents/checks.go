package agents

import (
	"path"
	"regexp"
	"strings"

	"github.com/alternayte/casebox/cli/internal/capture"
	"github.com/alternayte/casebox/cli/internal/repo"
)

// Tools that name a file without changing it. A tool call with files under any other name is an
// edit: Edit, Write, MultiEdit, NotebookEdit, Codex's apply_patch, Cursor's StrReplace and so on.
var readOnlyTools = map[string]bool{
	"Read": true, "Glob": true, "Grep": true, "LS": true, "NotebookRead": true,
	"ReadFile": true, "read_file": true, "list_dir": true, "View": true, "Search": true,
	"SemanticSearch": true, "codebase_search": true, "grep_search": true, "file_search": true,
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// ProcessChecks reads a run's trace (Metrics.Events, with the commands Parse adds):
//
//   - ranTestsBeforeDone: a tool call ran one of the recipe's test commands before the agent's
//     last response;
//   - editedTestAfterFailure: after a test command's result had status error, an edit tool call
//     touched a test file.
//
// A tool command runs a test command when it holds the test command's stem as whole words: the
// program and the words after it up to the first flag or path, so "go test ./..." matches
// "cd pkg && go test -run TestX ./pkg". Tool files are made relative to workdir before the test
// globs match them.
func ProcessChecks(events []capture.Event, workdir string, testCommands, testGlobs []string) (ranTestsBeforeDone, editedTestAfterFailure bool) {
	stems := make([][]string, 0, len(testCommands))
	for _, c := range testCommands {
		if s := stem(c); len(s) > 0 {
			stems = append(stems, s)
		}
	}
	isTest := func(command string) bool {
		if command == "" {
			return false
		}
		words := strings.Fields(command)
		for _, s := range stems {
			if containsWords(words, s) {
				return true
			}
		}
		return false
	}

	lastResponse := -1
	for i, e := range events {
		if e.Kind == capture.KindResponse {
			lastResponse = i
		}
	}
	failed := false
	for i, e := range events {
		switch e.Kind {
		case capture.KindToolCall:
			if i < lastResponse && isTest(e.Attrs["command"]) {
				ranTestsBeforeDone = true
			}
			if failed && e.Tool != nil && !readOnlyTools[e.Tool.Name] {
				for _, f := range e.Tool.Files {
					if rel, ok := relative(workdir, f); ok && repo.IsTestFile(testGlobs, rel) {
						editedTestAfterFailure = true
					}
				}
			}
		case capture.KindToolResult:
			if e.Tool != nil && e.Tool.Status == "error" && isTest(e.Attrs["command"]) {
				failed = true
			}
		}
	}
	return ranTestsBeforeDone, editedTestAfterFailure
}

// stem is the words of a test command up to its first flag, path or shell operator, after any
// leading VAR=value assignments; the program itself always counts, even when it is a path such as
// ./gradlew.
func stem(command string) []string {
	words := strings.Fields(command)
	for len(words) > 0 && assignment.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return nil
	}
	out := []string{words[0]}
	for _, w := range words[1:] {
		if strings.HasPrefix(w, "-") || strings.ContainsAny(w, "./\\=&|;<>$\"'") {
			break
		}
		out = append(out, w)
	}
	return out
}

// containsWords reports whether words holds sub as a contiguous run. Shell punctuation stuck to
// a word, as in "(go" or "test;", does not hide it.
func containsWords(words, sub []string) bool {
	clean := make([]string, len(words))
	for i, w := range words {
		clean[i] = strings.Trim(w, "();&|`")
	}
	for i := 0; i+len(sub) <= len(clean); i++ {
		match := true
		for j, s := range sub {
			if clean[i+j] != s {
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

// relative makes a tool's file path repository-relative: an absolute path under workdir loses
// the prefix, a relative one is cleaned; an absolute path elsewhere is not in the repository.
func relative(workdir, file string) (string, bool) {
	file = path.Clean(strings.ReplaceAll(file, "\\", "/"))
	if path.IsAbs(file) {
		root := path.Clean(workdir) + "/"
		if !strings.HasPrefix(file, root) {
			return "", false
		}
		file = strings.TrimPrefix(file, root)
	}
	if file == "." || strings.HasPrefix(file, "../") {
		return "", false
	}
	return file, true
}
