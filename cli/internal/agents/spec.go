// Package agents runs coding agents headless inside a sandbox for an evaluation
// (docs/specs/evaluations.md, "Agent adapters"). An adapter names the install steps of its
// pinned version, builds the argv of its headless mode, names the environment and the model API
// hosts it needs, and parses its own session log into metrics and a trace. Flags are pinned per
// agent version in the table in flags.go; an unknown version is refused.
package agents

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Agent names.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
	CursorCLI  = "cursor-cli"
	CommandCLI = "command"
)

// Log formats a command template can name.
const LogFormatNone = "none"

// Spec is one side of an evaluation: the harness specification of SDD section 8.
type Spec struct {
	Agent        string   `json:"agent"`
	AgentVersion string   `json:"agentVersion"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort,omitempty"`
	Harness      string   `json:"harness"`
	Settings     Settings `json:"settings"`
	Command      *Command `json:"command"`
	Shared       *Shared  `json:"shared,omitempty"`
	// Overrides is the blob hash of a proposal candidate's harness files, laid over the harness
	// at Harness (docs/specs/self-evolution.md).
	Overrides string `json:"overrides,omitempty"`
}

// Shared is a shared harness repository at a ref (casebox.yml harness.shared). Its files go into
// the agent's user-level configuration in the sandbox home (docs/specs/harness-ci.md).
type Shared struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
}

// Settings cap a run. A nil field is unset: no max turns, the default timeout, no token cap.
type Settings struct {
	MaxTurns       *int   `json:"maxTurns,omitempty"`
	TimeoutMinutes *int   `json:"timeoutMinutes,omitempty"`
	TokenCap       *int64 `json:"tokenCap,omitempty"`
}

// DefaultTimeout is the wall-clock limit of a run whose settings name none.
const DefaultTimeout = 30 * time.Minute

// Timeout is the run's wall-clock limit, with its default.
func (s Settings) Timeout() time.Duration {
	if s.TimeoutMinutes == nil {
		return DefaultTimeout
	}
	return time.Duration(*s.TimeoutMinutes) * time.Minute
}

// Command is the template of the command agent: any headless CLI. {instruction_file} and
// {model} are replaced, shell-quoted. LogGlob finds its session log (relative to the sandbox
// user's home unless absolute) and LogFormat names the parser: claude-code, codex, cursor-cli or
// none.
type Command struct {
	Template  string `json:"template"`
	LogGlob   string `json:"logGlob,omitempty"`
	LogFormat string `json:"logFormat"`
}

// Changes names what differs between two sides, in the order agent, agentVersion, model, effort,
// harness, settings, command. An evaluation changes exactly one of them; the server repeats this
// rule, and the CLI uses it to refuse early.
func Changes(a, b Spec) []string {
	var out []string
	if a.Agent != b.Agent {
		out = append(out, "agent")
	}
	if a.AgentVersion != b.AgentVersion {
		out = append(out, "agentVersion")
	}
	if a.Model != b.Model {
		out = append(out, "model")
	}
	if a.Effort != b.Effort {
		out = append(out, "effort")
	}
	if a.Harness != b.Harness {
		out = append(out, "harness")
	}
	if !reflect.DeepEqual(a.Settings, b.Settings) {
		out = append(out, "settings")
	}
	if !reflect.DeepEqual(a.Command, b.Command) {
		out = append(out, "command")
	}
	return out
}

// CheckOneChange refuses two sides that do not differ in exactly one thing.
func CheckOneChange(baseline, candidate Spec) error {
	changes := Changes(baseline, candidate)
	switch len(changes) {
	case 1:
		return nil
	case 0:
		return errors.New("the two sides are the same; an evaluation changes exactly one thing")
	default:
		return fmt.Errorf("the two sides differ in %s; an evaluation changes exactly one thing", strings.Join(changes, ", "))
	}
}

// MutableModel reports whether a model ID names no fixed version: it contains "latest", or its
// name (after the last "/") has no digit, as an alias such as "sonnet" has none. The server
// applies the same rule.
func MutableModel(model string) bool {
	m := strings.ToLower(model)
	if strings.Contains(m, "latest") {
		return true
	}
	name := m[strings.LastIndex(m, "/")+1:]
	return !strings.ContainsAny(name, "0123456789")
}

// Validate refuses a spec the adapters cannot run as written: an unknown agent or version, an
// effort or max turns the version does not take, or a malformed command template.
func Validate(s Spec) error {
	if strings.TrimSpace(s.Model) == "" {
		return errors.New("the spec names no model")
	}
	if strings.ContainsAny(s.Model, "\x00\n\r") {
		return fmt.Errorf("the model %q is not one line", s.Model)
	}
	if strings.TrimSpace(s.Harness) == "" {
		return errors.New("the spec names no harness: a git ref, or none")
	}
	if sh := s.Shared; sh != nil {
		if s.Agent != ClaudeCode && s.Agent != Codex {
			return fmt.Errorf("a shared harness goes into the agent's user-level configuration, and %s has none that Casebox knows; only %s and %s do", s.Agent, ClaudeCode, Codex)
		}
		if strings.TrimSpace(sh.Repo) == "" || strings.TrimSpace(sh.Ref) == "" || strings.HasPrefix(sh.Ref, "-") {
			return errors.New("the shared harness names no repository or ref")
		}
	}
	if v := s.Settings.MaxTurns; v != nil && *v < 1 {
		return fmt.Errorf("settings.maxTurns is %d; it must be at least 1", *v)
	}
	if v := s.Settings.TimeoutMinutes; v != nil && *v < 1 {
		return fmt.Errorf("settings.timeoutMinutes is %d; it must be at least 1", *v)
	}
	if v := s.Settings.TokenCap; v != nil && *v < 1 {
		return fmt.Errorf("settings.tokenCap is %d; it must be at least 1", *v)
	}

	if s.Agent == CommandCLI {
		return validateCommand(s)
	}
	if s.Command != nil {
		return fmt.Errorf("a command template goes only with the agent %q, not %q", CommandCLI, s.Agent)
	}
	row, err := lookup(s.Agent, s.AgentVersion)
	if err != nil {
		return err
	}
	if s.Effort != "" && !contains(row.Efforts, s.Effort) {
		if len(row.Efforts) == 0 {
			return fmt.Errorf("%s %s takes no effort; leave effort empty", s.Agent, s.AgentVersion)
		}
		return fmt.Errorf("%s %s takes effort %s, not %q", s.Agent, s.AgentVersion, strings.Join(row.Efforts, ", "), s.Effort)
	}
	if s.Settings.MaxTurns != nil && !row.MaxTurns {
		return fmt.Errorf("%s %s has no max-turns control; leave settings.maxTurns unset", s.Agent, s.AgentVersion)
	}
	if s.Agent == CursorCLI && s.Effort != "" && strings.Contains(s.Model, "[") {
		return fmt.Errorf("the model %q already carries bracket parameters; name the effort there or in effort, not both", s.Model)
	}
	return nil
}

func validateCommand(s Spec) error {
	c := s.Command
	if c == nil {
		return errors.New("the command agent needs a command template")
	}
	if strings.TrimSpace(s.AgentVersion) == "" {
		return errors.New("the command agent needs an agentVersion that names the CLI's version")
	}
	if !strings.Contains(c.Template, "{instruction_file}") {
		return errors.New("the command template must contain {instruction_file}")
	}
	switch c.LogFormat {
	case ClaudeCode, Codex, CursorCLI:
		if strings.TrimSpace(c.LogGlob) == "" {
			return fmt.Errorf("the command template's log format is %s but it names no logGlob", c.LogFormat)
		}
	case LogFormatNone:
	default:
		return fmt.Errorf("the command template's log format %q is not claude-code, codex, cursor-cli or none", c.LogFormat)
	}
	if s.Effort != "" {
		return errors.New("the command agent takes no effort: put it in the template")
	}
	if s.Settings.MaxTurns != nil {
		return errors.New("the command agent has no max-turns control: put it in the template, and leave settings.maxTurns unset")
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
