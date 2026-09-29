package agents

import (
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Adapter runs one spec's agent. For builds it after Validate; its methods switch on the agent.
type Adapter struct {
	spec Spec
	row  Row // the flag table row; zero for the command agent
}

// For validates a spec and returns its adapter.
func For(spec Spec) (Adapter, error) {
	if err := Validate(spec); err != nil {
		return Adapter{}, err
	}
	a := Adapter{spec: spec}
	if spec.Agent != CommandCLI {
		row, err := lookup(spec.Agent, spec.AgentVersion)
		if err != nil {
			return Adapter{}, err
		}
		a.row = row
	}
	return a, nil
}

// Spec is the spec the adapter was built for.
func (a Adapter) Spec() Spec { return a.spec }

// Row is the flag table row the adapter uses; zero for the command agent.
func (a Adapter) Row() Row { return a.row }

// Install is the shell steps, run as root by /bin/sh at image build time, that put the pinned
// agent version on PATH for every user. They are appended to the recipe's install steps, so the
// image cache key includes the agent and version. Each needs curl and tar in the image.
//
//   - Claude Code: the native build, fetched by version from the release bucket the official
//     installer (claude.ai/install.sh) uses, checked against the SHA-256 in that version's
//     manifest, and put in /usr/local/bin. The installer itself is not used: it installs into
//     root's home, which the sandbox user cannot run from. musl images get the musl build,
//     which needs libgcc and libstdc++.
//   - Codex: the static musl binary of the pinned GitHub release, for the image's architecture.
//   - Cursor CLI: the versioned package that Cursor's installer (cursor.com/install) downloads.
//     The installer cannot pin a version: it is generated for the latest build. The package
//     bundles its own Node for glibc and starts from a bash script, so the image needs glibc
//     and bash.
//   - command: nothing; the recipe installs the CLI.
func (a Adapter) Install() []string {
	v := a.spec.AgentVersion
	switch a.spec.Agent {
	case ClaudeCode:
		return []string{`set -eu
command -v curl >/dev/null || { echo "installing Claude Code needs curl in the image" >&2; exit 1; }
case "$(uname -m)" in x86_64|amd64) arch=x64 ;; aarch64|arm64) arch=arm64 ;; *) echo "Claude Code has no build for $(uname -m)" >&2; exit 1 ;; esac
if [ -f /lib/libc.musl-x86_64.so.1 ] || [ -f /lib/libc.musl-aarch64.so.1 ]; then platform="linux-$arch-musl"; else platform="linux-$arch"; fi
base="https://downloads.claude.ai/claude-code-releases/` + v + `"
sum="$(curl -fsSL "$base/manifest.json" | tr -d ' \t\r\n' | sed -n "s/.*\"$platform\":{[^{}]*\"checksum\":\"\([0-9a-f]\{64\}\)\".*/\1/p")"
[ -n "$sum" ] || { echo "the Claude Code ` + v + ` manifest has no $platform build" >&2; exit 1; }
curl -fsSL -o /usr/local/bin/claude "$base/$platform/claude"
echo "$sum  /usr/local/bin/claude" | sha256sum -c - >/dev/null || { rm -f /usr/local/bin/claude; echo "the Claude Code download does not match its checksum" >&2; exit 1; }
chmod 0755 /usr/local/bin/claude`}
	case Codex:
		return []string{`set -eu
command -v curl >/dev/null || { echo "installing Codex needs curl in the image" >&2; exit 1; }
case "$(uname -m)" in x86_64|amd64) target=x86_64-unknown-linux-musl ;; aarch64|arm64) target=aarch64-unknown-linux-musl ;; *) echo "Codex has no build for $(uname -m)" >&2; exit 1 ;; esac
tmp="$(mktemp -d)"
curl -fsSL "https://github.com/openai/codex/releases/download/rust-v` + v + `/codex-$target.tar.gz" | tar -xzf - -C "$tmp"
mv "$tmp/codex-$target" /usr/local/bin/codex
chmod 0755 /usr/local/bin/codex
rm -rf "$tmp"`}
	case CursorCLI:
		return []string{`set -eu
command -v curl >/dev/null || { echo "installing the Cursor CLI needs curl in the image" >&2; exit 1; }
command -v bash >/dev/null || { echo "the Cursor CLI needs bash in the image" >&2; exit 1; }
case "$(uname -m)" in x86_64|amd64) arch=x64 ;; aarch64|arm64) arch=arm64 ;; *) echo "the Cursor CLI has no build for $(uname -m)" >&2; exit 1 ;; esac
tmp="$(mktemp -d)"
curl -fsSL "https://downloads.cursor.com/lab/` + v + `/linux/$arch/agent-cli-package.tar.gz" | tar -xzf - -C "$tmp"
set -- "$tmp"/*
[ "$#" -eq 1 ] && [ -x "$1/cursor-agent" ] || { echo "the Cursor CLI package does not hold one directory with cursor-agent" >&2; exit 1; }
mkdir -p /usr/local/lib/cursor-agent
rm -rf "/usr/local/lib/cursor-agent/` + v + `"
mv "$1" "/usr/local/lib/cursor-agent/` + v + `"
chmod -R a+rX "/usr/local/lib/cursor-agent/` + v + `"
ln -sf "/usr/local/lib/cursor-agent/` + v + `/cursor-agent" /usr/local/bin/cursor-agent
rm -rf "$tmp"`}
	default:
		return nil
	}
}

// The argv wrappers. Each changes to the working directory given as $1 and reads the instruction
// file given as $2; the agent's argv follows. Claude Code and Codex read the instruction from
// stdin; the Cursor CLI takes it as its prompt argument, which Linux caps at 128 KiB.
const (
	stdinScript  = `cd -- "$1" && f=$2 && shift 2 && exec "$@" < "$f"`
	codexScript  = `cd -- "$1" && f=$2 && shift 2 && if [ -n "${OPENAI_BASE_URL:-}" ]; then set -- "$@" -c "openai_base_url=\"$OPENAI_BASE_URL\""; fi && exec "$@" - < "$f"`
	promptScript = `cd -- "$1" && p=$(cat -- "$2") && shift 2 && exec "$@" "$p"`
)

// Argv is the headless command: the model, effort and max turns of the spec, permissions skipped
// because the sandbox is the boundary, and the instruction from instructionFile (a path in the
// sandbox, outside the working directory). It runs in workdir as the sandbox user.
func (a Adapter) Argv(instructionFile, workdir string) []string {
	s := a.spec
	wrap := func(script string, argv ...string) []string {
		return append([]string{"sh", "-c", script, "sh", workdir, instructionFile}, argv...)
	}
	switch s.Agent {
	case ClaudeCode:
		argv := append([]string{"claude"}, a.row.Headless...)
		argv = append(argv, "--model", s.Model)
		if s.Effort != "" {
			argv = append(argv, "--effort", s.Effort)
		}
		if s.Settings.MaxTurns != nil {
			argv = append(argv, "--max-turns", strconv.Itoa(*s.Settings.MaxTurns))
		}
		return wrap(stdinScript, argv...)
	case Codex:
		argv := append([]string{"codex"}, a.row.Headless...)
		argv = append(argv, "-C", workdir, "-m", s.Model)
		if s.Effort != "" {
			argv = append(argv, "-c", `model_reasoning_effort="`+s.Effort+`"`)
		}
		return wrap(codexScript, argv...)
	case CursorCLI:
		model := s.Model
		if s.Effort != "" {
			model += "[effort=" + s.Effort + "]"
		}
		argv := append([]string{"cursor-agent"}, a.row.Headless...)
		argv = append(argv, "--workspace", workdir, "--model", model)
		return wrap(promptScript, argv...)
	default:
		script := strings.NewReplacer("{instruction_file}", shellQuote(instructionFile), "{model}", shellQuote(s.Model)).Replace(s.Command.Template)
		return []string{"sh", "-c", "cd -- " + shellQuote(workdir) + " && " + script}
	}
}

// The environment each agent reads from the worker: model keys and base URLs.
var providerEnv = map[string][]string{
	ClaudeCode: {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"},
	Codex:      {"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"},
	CursorCLI:  {"CURSOR_API_KEY", "CURSOR_API_ENDPOINT"},
}

// The variables of which one must be set for the agent to authenticate.
var providerKeys = map[string][]string{
	ClaudeCode: {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
	Codex:      {"OPENAI_API_KEY", "CODEX_API_KEY"},
	CursorCLI:  {"CURSOR_API_KEY"},
}

// The base URL variable and default hosts of each provider.
var (
	providerBase  = map[string]string{ClaudeCode: "ANTHROPIC_BASE_URL", Codex: "OPENAI_BASE_URL", CursorCLI: "CURSOR_API_ENDPOINT"}
	providerHosts = map[string][]string{
		ClaudeCode: {"api.anthropic.com"},
		Codex:      {"api.openai.com"},
		// api2 serves the agent; repo42 serves codebase indexing. api3 takes metrics only.
		CursorCLI: {"api2.cursor.sh", "repo42.cursor.sh"},
	}
)

// providers are the model providers whose environment the adapter passes: its own, or for the
// command agent every provider whose key the worker holds.
func (a Adapter) providers(worker map[string]string) []string {
	if a.spec.Agent != CommandCLI {
		return []string{a.spec.Agent}
	}
	var out []string
	for _, p := range []string{ClaudeCode, Codex, CursorCLI} {
		for _, k := range providerKeys[p] {
			if worker[k] != "" {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// Env is the environment the agent needs from the worker's: its model key and base URL, plus the
// settings that keep it from updating itself or calling home. It refuses a worker that holds no
// key for the agent. The keys go into the agent sandbox only.
func (a Adapter) Env(worker map[string]string) (map[string]string, error) {
	providers := a.providers(worker)
	if len(providers) == 0 {
		return nil, cbx.Errorf(cbx.NoModelKey, "the worker holds no model key: set ANTHROPIC_API_KEY, OPENAI_API_KEY or CURSOR_API_KEY")
	}
	env := map[string]string{}
	for _, p := range providers {
		keyed := false
		for _, k := range providerEnv[p] {
			if v := worker[k]; v != "" {
				env[k] = v
			}
		}
		for _, k := range providerKeys[p] {
			keyed = keyed || worker[k] != ""
		}
		if !keyed {
			return nil, fmt.Errorf("the worker holds no key for %s: set %s", p, strings.Join(providerKeys[p], " or "))
		}
	}
	switch a.spec.Agent {
	case ClaudeCode:
		env["DISABLE_AUTOUPDATER"] = "1"
		env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	case Codex:
		// codex exec authenticates with CODEX_API_KEY; the OpenAI provider reads OPENAI_API_KEY.
		if env["CODEX_API_KEY"] == "" {
			env["CODEX_API_KEY"] = env["OPENAI_API_KEY"]
		}
		if env["OPENAI_API_KEY"] == "" {
			env["OPENAI_API_KEY"] = env["CODEX_API_KEY"]
		}
	}
	return env, nil
}

// Hosts is the model API hosts the agent reaches, for the egress allow-list: the base URL's host
// when env sets one, else the provider's defaults. env is what Env returned.
func (a Adapter) Hosts(env map[string]string) ([]string, error) {
	var hosts []string
	for _, p := range a.providers(env) {
		base := env[providerBase[p]]
		if base == "" {
			hosts = append(hosts, providerHosts[p]...)
			continue
		}
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
			return nil, fmt.Errorf("%s %q is not an http or https URL", providerBase[p], base)
		}
		if port := u.Port(); port != "" && port != "443" && port != "80" {
			return nil, fmt.Errorf("%s %q uses port %s; the egress proxy allows only 443 and 80", providerBase[p], base, port)
		}
		hosts = append(hosts, strings.ToLower(u.Hostname()))
	}
	sort.Strings(hosts)
	return hosts, nil
}

// LogGlob is where the agent's session log lands, under the sandbox user's home. It is empty
// for a command template whose log format is none.
//
//   - Claude Code: ~/.claude/projects/<project>/<session>.jsonl (subagent transcripts sit one
//     level deeper and are not matched).
//   - Codex: ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl.
//   - Cursor CLI: ~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl.
func (a Adapter) LogGlob(home string) string {
	switch a.spec.Agent {
	case ClaudeCode:
		return path.Join(home, ".claude/projects/*/*.jsonl")
	case Codex:
		return path.Join(home, ".codex/sessions/*/*/*/rollout-*.jsonl")
	case CursorCLI:
		return path.Join(home, ".cursor/projects/*/agent-transcripts/*/*.jsonl")
	default:
		c := a.spec.Command
		if c.LogFormat == LogFormatNone {
			return ""
		}
		if path.IsAbs(c.LogGlob) {
			return c.LogGlob
		}
		return path.Join(home, c.LogGlob)
	}
}

// logFormat is the parser of the agent's session log.
func (a Adapter) logFormat() string {
	if a.spec.Agent == CommandCLI {
		return a.spec.Command.LogFormat
	}
	return a.spec.Agent
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
