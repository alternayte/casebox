package agents

import (
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func intp(n int) *int       { return &n }
func int64p(n int64) *int64 { return &n }

// The argv of every row of the flag table is pinned: a flag a version does not accept makes the
// agent exit before it starts, which shows up only as a failed run.
func TestArgvPinnedPerRow(t *testing.T) {
	want := map[string][]string{
		ClaudeCode: {"sh", "-c", stdinScript, "sh", "/workspace", "/tmp/casebox/instruction.md",
			"claude", "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions",
			"--model", "claude-sonnet-5", "--effort", "high", "--max-turns", "200"},
		Codex: {"sh", "-c", codexScript, "sh", "/workspace", "/tmp/casebox/instruction.md",
			"codex", "exec", "--json", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check", "--color", "never",
			"-C", "/workspace", "-m", "gpt-5.5", "-c", `model_reasoning_effort="high"`},
		CursorCLI: {"sh", "-c", promptScript, "sh", "/workspace", "/tmp/casebox/instruction.md",
			"cursor-agent", "-p", "--force", "--trust", "--sandbox", "disabled", "--approve-mcps", "--output-format", "stream-json",
			"--workspace", "/workspace", "--model", "gpt-5.5[effort=high]"},
	}
	models := map[string]string{ClaudeCode: "claude-sonnet-5", Codex: "gpt-5.5", CursorCLI: "gpt-5.5"}
	for _, row := range Flags {
		for _, version := range row.Verified {
			spec := Spec{Agent: row.Agent, AgentVersion: version, Model: models[row.Agent], Effort: "high", Harness: "main"}
			if row.MaxTurns {
				spec.Settings.MaxTurns = intp(200)
			}
			a, err := For(spec)
			if err != nil {
				t.Fatalf("%s %s: %v", row.Agent, version, err)
			}
			if got := a.Argv("/tmp/casebox/instruction.md", "/workspace"); !reflect.DeepEqual(got, want[row.Agent]) {
				t.Errorf("%s %s argv =\n%q\nwant\n%q", row.Agent, version, got, want[row.Agent])
			}
		}
	}
}

func TestArgvWithoutEffortOrMaxTurns(t *testing.T) {
	a, err := For(Spec{Agent: ClaudeCode, AgentVersion: "2.1.284", Model: "claude-sonnet-5", Harness: "none"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(a.Argv("/i", "/w")[6:], " ")
	if got != "claude -p --output-format stream-json --verbose --dangerously-skip-permissions --model claude-sonnet-5" {
		t.Fatalf("argv = %s", got)
	}
}

func TestCommandTemplateArgv(t *testing.T) {
	spec := Spec{Agent: CommandCLI, AgentVersion: "0.9.1", Model: "o'model", Harness: "main",
		Command: &Command{Template: "pi --print --model {model} < {instruction_file}", LogGlob: ".pi/sessions/*.jsonl", LogFormat: ClaudeCode}}
	a, err := For(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sh", "-c", `cd -- '/workspace' && pi --print --model 'o'\''model' < '/tmp/i.md'`}
	if got := a.Argv("/tmp/i.md", "/workspace"); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if got := a.LogGlob("/home/casebox"); got != "/home/casebox/.pi/sessions/*.jsonl" {
		t.Fatalf("log glob = %s", got)
	}
}

// The wrappers must hand the instruction to the agent unchanged: on stdin, or as one argument.
func TestWrappersDeliverTheInstruction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wrappers run in a Linux sandbox")
	}
	dir := t.TempDir()
	file := dir + "/instruction.md"
	instruction := "Fix the parser.\nIt's \"quoted\" and $HOME stays literal."
	if err := os.WriteFile(file, []byte(instruction), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"stdin":  {"sh", "-c", stdinScript, "sh", dir, file, "sh", "-c", `pwd; cat`},
		"prompt": {"sh", "-c", promptScript, "sh", dir, file, "sh", "-c", `pwd; printf %s "$1"`, "agent"},
		"codex":  {"sh", "-c", codexScript, "sh", dir, file, "sh", "-c", `pwd; echo "$@"; cat`, "agent"},
	}
	for name, argv := range cases {
		out, err := exec.Command(argv[0], argv[1:]...).Output()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := string(out)
		if !strings.HasSuffix(got, instruction) || !strings.Contains(got, dir) {
			t.Errorf("%s: output = %q", name, got)
		}
		if name == "codex" && !strings.Contains(got, "\n-\n") {
			t.Errorf("codex: the prompt argument is not -: %q", got)
		}
	}
	cmd := exec.Command("sh", "-c", codexScript, "sh", dir, file, "sh", "-c", `echo "$@"`, "agent")
	cmd.Env = append(cmd.Environ(), "OPENAI_BASE_URL=https://llm.example.com/v1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != `-c openai_base_url="https://llm.example.com/v1" -` {
		t.Fatalf("codex base URL: %q", out)
	}
}

// The install steps run under /bin/sh at image build time; a syntax error fails every image.
func TestInstallStepsParse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the install steps run in a Linux image")
	}
	for _, row := range Flags {
		v := row.Verified[len(row.Verified)-1]
		a, err := For(Spec{Agent: row.Agent, AgentVersion: v, Model: "m-1", Harness: "main"})
		if err != nil {
			t.Fatal(err)
		}
		steps := a.Install()
		if len(steps) == 0 {
			t.Fatalf("%s has no install steps", row.Agent)
		}
		for _, s := range steps {
			if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
				t.Errorf("%s install step does not parse: %v\n%s", row.Agent, err, out)
			}
			if !strings.Contains(s, v) {
				t.Errorf("%s install step does not pin %s", row.Agent, v)
			}
		}
	}
}

func TestValidate(t *testing.T) {
	ok := Spec{Agent: ClaudeCode, AgentVersion: "2.1.284", Model: "claude-sonnet-5", Harness: "main"}
	cases := []struct {
		name string
		edit func(*Spec)
		want string // a fragment of the error; empty for none
	}{
		{"valid", func(*Spec) {}, ""},
		{"unknown agent", func(s *Spec) { s.Agent = "aider" }, `unknown agent "aider"`},
		{"unknown version lists the known ones", func(s *Spec) { s.AgentVersion = "2.2.0" }, "known versions: 2.1.273 to 2.1.284"},
		{"version below the table", func(s *Spec) { s.AgentVersion = "2.1.272" }, "not in the flag table"},
		{"malformed version", func(s *Spec) { s.AgentVersion = "latest" }, "not a release version"},
		{"effort the version lacks", func(s *Spec) { s.Effort = "ultra" }, "takes effort low, medium, high, xhigh, max"},
		{"no model", func(s *Spec) { s.Model = " " }, "no model"},
		{"no harness", func(s *Spec) { s.Harness = "" }, "no harness"},
		{"zero timeout", func(s *Spec) { s.Settings.TimeoutMinutes = intp(0) }, "timeoutMinutes"},
		{"zero cap", func(s *Spec) { s.Settings.TokenCap = int64p(0) }, "tokenCap"},
		{"template on a named agent", func(s *Spec) { s.Command = &Command{Template: "x {instruction_file}", LogFormat: "none"} }, "goes only with"},
		{"codex max turns", func(s *Spec) { s.Agent, s.AgentVersion, s.Settings.MaxTurns = Codex, "0.153.4", intp(50) }, "no max-turns control"},
		{"codex effort", func(s *Spec) { s.Agent, s.AgentVersion, s.Effort = Codex, "0.159.0", "xhigh" }, ""},
		{"codex prerelease", func(s *Spec) { s.Agent, s.AgentVersion = Codex, "0.160.0-alpha.6" }, "not a release version"},
		{"cursor build", func(s *Spec) { s.Agent, s.AgentVersion = CursorCLI, "2026.09.28-64d2043" }, ""},
		{"cursor bare date", func(s *Spec) { s.Agent, s.AgentVersion = CursorCLI, "2026.09.28" }, "not a build version"},
		{"cursor newer build", func(s *Spec) { s.Agent, s.AgentVersion = CursorCLI, "2026.10.02-0a1b2c3" }, "known versions: 2026.06.19 to 2026.09.28"},
		{"cursor effort twice", func(s *Spec) {
			s.Agent, s.AgentVersion, s.Model, s.Effort = CursorCLI, "2026.09.28-64d2043", "claude-opus-4-8[effort=low]", "high"
		}, "bracket parameters"},
		{"command without placeholder", func(s *Spec) {
			s.Agent, s.Command = CommandCLI, &Command{Template: "pi --print", LogFormat: "none"}
		}, "{instruction_file}"},
		{"command without log glob", func(s *Spec) {
			s.Agent, s.Command = CommandCLI, &Command{Template: "pi {instruction_file}", LogFormat: Codex}
		}, "no logGlob"},
		{"command with unknown format", func(s *Spec) {
			s.Agent, s.Command = CommandCLI, &Command{Template: "pi {instruction_file}", LogFormat: "pi"}
		}, "not claude-code, codex, cursor-cli or none"},
		{"command with effort", func(s *Spec) {
			s.Agent, s.Effort, s.Command = CommandCLI, "high", &Command{Template: "pi {instruction_file}", LogFormat: "none"}
		}, "takes no effort"},
	}
	for _, c := range cases {
		s := ok
		c.edit(&s)
		err := Validate(s)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error = %v, want one containing %q", c.name, err, c.want)
		}
	}
}

func TestMutableModel(t *testing.T) {
	cases := map[string]bool{
		"sonnet":                     true,
		"opus":                       true,
		"claude-sonnet-latest":       true,
		"openai/gpt-latest":          true,
		"anthropic/claude":           true,
		"claude-sonnet-5":            false,
		"claude-sonnet-5-20260901":   false,
		"gpt-5.5":                    false,
		"anthropic/claude-opus-4-8":  false,
		"models/gemini-3-pro":        false,
		"o3":                         false,
		"Claude-Sonnet-LATEST-20260": true,
	}
	for model, want := range cases {
		if got := MutableModel(model); got != want {
			t.Errorf("MutableModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestChanges(t *testing.T) {
	base := Spec{Agent: ClaudeCode, AgentVersion: "2.1.284", Model: "claude-sonnet-5", Effort: "high", Harness: "main",
		Settings: Settings{MaxTurns: intp(200), TimeoutMinutes: intp(30), TokenCap: int64p(2_000_000)}}
	same := base
	same.Settings = Settings{MaxTurns: intp(200), TimeoutMinutes: intp(30), TokenCap: int64p(2_000_000)}
	if got := Changes(base, same); len(got) != 0 {
		t.Fatalf("equal specs differ in %v", got)
	}
	if err := CheckOneChange(base, same); err == nil {
		t.Fatal("two equal sides were accepted")
	}

	harness := base
	harness.Harness = "none"
	if got := Changes(base, harness); !reflect.DeepEqual(got, []string{"harness"}) {
		t.Fatalf("changes = %v", got)
	}
	if err := CheckOneChange(base, harness); err != nil {
		t.Fatal(err)
	}

	two := base
	two.Model, two.Settings.TokenCap = "claude-opus-5", int64p(1)
	if got := Changes(base, two); !reflect.DeepEqual(got, []string{"model", "settings"}) {
		t.Fatalf("changes = %v", got)
	}
	if err := CheckOneChange(base, two); err == nil || !strings.Contains(err.Error(), "model, settings") {
		t.Fatalf("error = %v", err)
	}

	cmd := base
	cmd.Command = &Command{Template: "x {instruction_file}", LogFormat: "none"}
	if got := Changes(base, cmd); !reflect.DeepEqual(got, []string{"command"}) {
		t.Fatalf("changes = %v", got)
	}
}

func TestEnvAndHosts(t *testing.T) {
	worker := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant", "OPENAI_API_KEY": "sk-oai", "CURSOR_API_KEY": "cur",
		"ANTHROPIC_BASE_URL": "https://gateway.example.com/anthropic", "UNRELATED": "x",
	}
	cases := []struct {
		spec  Spec
		env   map[string]string
		hosts []string
	}{
		{Spec{Agent: ClaudeCode, AgentVersion: "2.1.284", Model: "m-1", Harness: "main"},
			map[string]string{"ANTHROPIC_API_KEY": "sk-ant", "ANTHROPIC_BASE_URL": "https://gateway.example.com/anthropic", "DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"},
			[]string{"gateway.example.com"}},
		{Spec{Agent: Codex, AgentVersion: "0.159.0", Model: "m-1", Harness: "main"},
			map[string]string{"OPENAI_API_KEY": "sk-oai", "CODEX_API_KEY": "sk-oai"},
			[]string{"api.openai.com"}},
		{Spec{Agent: CursorCLI, AgentVersion: "2026.09.28-64d2043", Model: "m-1", Harness: "main"},
			map[string]string{"CURSOR_API_KEY": "cur"},
			[]string{"api2.cursor.sh", "repo42.cursor.sh"}},
		{Spec{Agent: CommandCLI, AgentVersion: "1.0.0", Model: "m-1", Harness: "main", Command: &Command{Template: "pi {instruction_file}", LogFormat: "none"}},
			map[string]string{"ANTHROPIC_API_KEY": "sk-ant", "ANTHROPIC_BASE_URL": "https://gateway.example.com/anthropic", "OPENAI_API_KEY": "sk-oai", "CURSOR_API_KEY": "cur"},
			[]string{"api.openai.com", "api2.cursor.sh", "gateway.example.com", "repo42.cursor.sh"}},
	}
	for _, c := range cases {
		a, err := For(c.spec)
		if err != nil {
			t.Fatal(err)
		}
		env, err := a.Env(worker)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(env, c.env) {
			t.Errorf("%s env = %v, want %v", c.spec.Agent, env, c.env)
		}
		hosts, err := a.Hosts(env)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(hosts, c.hosts) {
			t.Errorf("%s hosts = %v, want %v", c.spec.Agent, hosts, c.hosts)
		}
	}

	a, _ := For(Spec{Agent: Codex, AgentVersion: "0.159.0", Model: "m-1", Harness: "main"})
	if _, err := a.Env(map[string]string{"ANTHROPIC_API_KEY": "sk-ant"}); err == nil {
		t.Fatal("codex ran without an OpenAI key")
	}
	if _, err := a.Hosts(map[string]string{"OPENAI_API_KEY": "k", "OPENAI_BASE_URL": "https://llm.example.com:8443/v1"}); err == nil {
		t.Fatal("a base URL on a port the egress proxy refuses was accepted")
	}
	if _, err := a.Hosts(map[string]string{"OPENAI_API_KEY": "k", "OPENAI_BASE_URL": "llm.example.com"}); err == nil {
		t.Fatal("a base URL without a scheme was accepted")
	}
}

func TestLogGlobs(t *testing.T) {
	want := map[string]string{
		ClaudeCode: "/home/casebox/.claude/projects/*/*.jsonl",
		Codex:      "/home/casebox/.codex/sessions/*/*/*/rollout-*.jsonl",
		CursorCLI:  "/home/casebox/.cursor/projects/*/agent-transcripts/*/*.jsonl",
	}
	for _, row := range Flags {
		a, err := For(Spec{Agent: row.Agent, AgentVersion: row.Verified[0], Model: "m-1", Harness: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if got := a.LogGlob("/home/casebox"); got != want[row.Agent] {
			t.Errorf("%s log glob = %s", row.Agent, got)
		}
	}
}
