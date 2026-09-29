// Package cbx holds Casebox's error codes (docs/specs/operations.md, Error codes). Every code has
// a page in docs/src/content/docs/reference/errors/; checks/error-codes.sh keeps them in step.
package cbx

import (
	"errors"
	"fmt"
	"strings"
)

// Docs is where each code's page lives.
const Docs = "https://casebox-docs.pages.dev/reference/errors/"

// The CLI's own codes; the server's arrive in its answers.
const (
	Refused         = "CBX001"
	Unauthenticated = "CBX010"
	Forbidden       = "CBX011"
	Unreachable     = "CBX012"
	NotEnrolled     = "CBX020"
	BadConfig       = "CBX021"
	NoAnalysisModel = "CBX061"
	NoPromptMode    = "CBX090"
	NoGitEmail      = "CBX091"
	CodexHookTrust  = "CBX092"
)

// URL is a code's page.
func URL(code string) string { return Docs + strings.ToLower(code) + "/" }

// Error is an error with its code; its message ends with the code and the page.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("%v (%s: %s)", e.Err, e.Code, URL(e.Code)) }

func (e *Error) Unwrap() error { return e.Err }

// Errorf is fmt.Errorf with a code.
func Errorf(code, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// Wrap gives err a code, unless it already has one.
func Wrap(code string, err error) error {
	if err == nil {
		return nil
	}
	var coded *Error
	if errors.As(err, &coded) {
		return err
	}
	return &Error{Code: code, Err: err}
}

// Line is a message with its code and page, for output that is not an error.
func Line(code, message string) string { return fmt.Sprintf("%s (%s: %s)", message, code, URL(code)) }
