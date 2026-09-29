package agents

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Row pins the headless flags of one agent over an inclusive version range. Verified names the
// versions whose --help the row was read from; the versions between them are taken to accept the
// same flags, since both ends do.
type Row struct {
	Agent    string
	Min, Max string
	Verified []string
	Headless []string // the fixed flags of the headless mode, in argv order
	Efforts  []string // the effort values the version takes; none means effort is refused
	MaxTurns bool     // whether the version takes a max-turns flag
}

// Flags is the pinned flag table.
//
//   - Claude Code 2.1.273 to 2.1.284: -p (print mode) with stream-json, which needs --verbose;
//     --dangerously-skip-permissions; --model, --effort (low to max) and --max-turns (hidden
//     from --help, present in the binary). The instruction arrives on stdin.
//   - Codex 0.153.4 to 0.159.0: exec --json; --dangerously-bypass-approvals-and-sandbox;
//     --skip-git-repo-check, since a workspace of several repositories has no repository at its
//     root; -m; effort as -c model_reasoning_effort (the ReasoningEffort values of the binary).
//     Codex has no max-turns control. The instruction arrives on stdin through the "-" prompt.
//   - Cursor CLI 2026.06.19 to 2026.09.28: -p with --force (commands run unless denied), --trust
//     (no workspace trust prompt), --sandbox disabled and --approve-mcps (the sandbox is the
//     boundary, and MCP servers are part of the harness), stream-json, --workspace; effort as the
//     model's bracket parameter, model[effort=high]. No max-turns control. The instruction is
//     the prompt argument.
var Flags = []Row{
	{
		Agent: ClaudeCode, Min: "2.1.273", Max: "2.1.284",
		Verified: []string{"2.1.273", "2.1.280", "2.1.282", "2.1.283", "2.1.284"},
		Headless: []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"},
		Efforts:  []string{"low", "medium", "high", "xhigh", "max"},
		MaxTurns: true,
	},
	{
		Agent: Codex, Min: "0.153.4", Max: "0.159.0",
		Verified: []string{"0.153.4", "0.159.0"},
		Headless: []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check", "--color", "never"},
		Efforts:  []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
	},
	{
		Agent: CursorCLI, Min: "2026.06.19", Max: "2026.09.28",
		Verified: []string{"2026.06.19-20-24-33-653a7fb", "2026.09.28-64d2043"},
		Headless: []string{"-p", "--force", "--trust", "--sandbox", "disabled", "--approve-mcps", "--output-format", "stream-json"},
		Efforts:  []string{"low", "medium", "high", "xhigh", "max"},
	},
}

var (
	semver = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
	// Cursor versions are a build date and a commit: 2026.09.28-64d2043, or with the build time,
	// 2026.06.19-20-24-33-653a7fb. The whole string names the download.
	cursorVersion = regexp.MustCompile(`^(\d{4})\.(\d{2})\.(\d{2})-(?:\d{2}-\d{2}-\d{2}-)?[0-9a-f]{7,40}$`)
	cursorDate    = regexp.MustCompile(`^(\d{4})\.(\d{2})\.(\d{2})$`)
)

// lookup finds the row that covers an agent version, or refuses it with the known ranges.
func lookup(agent, version string) (Row, error) {
	var known []string
	agentKnown := false
	for _, r := range Flags {
		if r.Agent != agent {
			continue
		}
		agentKnown = true
		known = append(known, r.Min+" to "+r.Max)
		in, err := inRange(agent, version, r.Min, r.Max)
		if err != nil {
			return Row{}, err
		}
		if in {
			return r, nil
		}
	}
	if !agentKnown {
		return Row{}, fmt.Errorf("unknown agent %q: use %s, %s, %s or %s", agent, ClaudeCode, Codex, CursorCLI, CommandCLI)
	}
	return Row{}, fmt.Errorf("%s version %q is not in the flag table; known versions: %s", agent, version, strings.Join(known, "; "))
}

func inRange(agent, version, lo, hi string) (bool, error) {
	v, err := versionKey(agent, version, false)
	if err != nil {
		return false, err
	}
	l, _ := versionKey(agent, lo, true)
	h, _ := versionKey(agent, hi, true)
	return compareKeys(l, v) <= 0 && compareKeys(v, h) <= 0, nil
}

// versionKey is the ordered numbers of a version: major, minor, patch, or Cursor's date. A range
// bound (and only a bound) may be a bare date for Cursor.
func versionKey(agent, version string, bound bool) ([3]int, error) {
	var parts []string
	if agent == CursorCLI {
		if m := cursorVersion.FindStringSubmatch(version); m != nil {
			parts = m[1:4]
		} else if m := cursorDate.FindStringSubmatch(version); bound && m != nil {
			parts = m[1:4]
		} else {
			return [3]int{}, fmt.Errorf("cursor-cli version %q is not a build version such as 2026.09.28-64d2043", version)
		}
	} else {
		m := semver.FindStringSubmatch(version)
		if m == nil {
			return [3]int{}, fmt.Errorf("%s version %q is not a release version such as 1.2.3", agent, version)
		}
		parts = m[1:4]
	}
	var k [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, err
		}
		k[i] = n
	}
	return k, nil
}

func compareKeys(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
