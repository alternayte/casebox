package steering

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// PromptVersion names the prompt below. Change it with any change to the prompt or the schema: a
// classification is recorded once per intervention, model and prompt version.
const PromptVersion = "steering-v1"

const (
	turnLimit        = 6000 // characters of an agent turn in a window
	diffLimit        = 8000 // characters of diff across a window's commits
	exampleTurnLimit = 600  // characters of an agent turn in a team example
	exampleLimit     = 20
)

const systemPrompt = `You classify one human intervention in the work of an AI coding agent. A team uses the labels to measure how often people must steer their agents, and why.

You see a window around the intervention: the agent's last turn before it, what the human did or wrote, and the agent's next turn. For interventions before or after a merge you see commits instead: their messages and diffs. Every person is written as [person]; you never know who anyone is. Text may be missing when the team does not capture prompts; then judge from the structure, the tools and the diffs.

Answer in three steps.

Step 1, intent. What did the human do?
- correction: fixes what the agent did. The agent produced wrong, incomplete, unwanted or unverified work, or went the wrong way, and the human steers it back. Rejecting or undoing the agent's action is a correction.
- direction: adds or changes scope. The agent's work so far was acceptable; the human asks for something new or for the next step.
- clarification: answers a question the agent asked, or gives information the agent asked for.
- routine: an expected approval or acknowledgement, such as "yes, go ahead", "continue", "thanks, commit it", with nothing to fix.
Only corrections count as steering. Never call a new request a correction. When the human both corrects and asks for more, it is a correction.

Step 2, only for a correction: what went wrong?
- missed_requirement: the agent left out or misread something the task or the human had asked for.
- broke_convention: the work runs against the repository's conventions, style, structure or team rules.
- wrong_approach: the design, algorithm or method is wrong, although the goal was understood.
- unverified_done: the agent claimed it was done, fixed or passing without running the tests, the build or the check that would show it.
- wrong_area: the agent changed files, modules or behaviour it should not have touched.
- over_engineered: the agent built more than needed: extra abstractions, options, layers or files.
- lacked_domain_knowledge: the agent did not know a business rule, a domain fact or how this system behaves.
- environment: a problem with tooling, the environment, dependencies, permissions or the machine, not with the agent's reasoning.
- other: none of these. Then give wentWrongLabel: a free label of at most 6 words.

Step 3, only for a correction: what would have prevented it?
- instruction: a rule in AGENTS.md, CLAUDE.md or the agent's rules files.
- skill: a written procedure or skill the agent could follow for this kind of task.
- tool_access: access to a tool, MCP server, system or data the agent did not have.
- verification: a check the agent should have run: a test, lint, build, just target or hook.
- clearer_ticket: a clearer task description or ticket from the human.
- stronger_model: only a more capable model would have avoided it.
- nothing: nothing in the harness would have helped.

For a direction, clarification or routine, leave wentWrong, wentWrongLabel and prevention null.

Give a confidence from 0 to 1 that your labels are right. Below 0.6 means you are unsure; say so rather than guess. Write reason first: one or two short sentences.`

const ruleNote = `The intent of this intervention is fixed by rule: it is a %s. Answer only step 2 and step 3.`

const taskPrompt = `You read the task a person gave an AI coding agent and name its type.
- bug: fix a defect or wrong behaviour.
- feature: add or change behaviour for users.
- refactor: restructure code without changing behaviour.
- test: add or fix tests.
- config: change build, CI, deployment, dependencies or settings.
- docs: write or change documentation.
Give a confidence from 0 to 1.`

// signalMeaning says what each signal is, so the model knows what the human did.
var signalMeaning = map[string]string{
	"follow_up":     "the human wrote a prompt after the agent had worked",
	"interruption":  "the human interrupted or stopped the agent",
	"denial":        "the human denied a tool call the agent wanted to make",
	"rewind":        "the human rewound the session to an earlier checkpoint",
	"human_edit":    "the human edited files between the agent's turns",
	"restarted":     "the human dropped the session without a commit and started again with another agent or model",
	"human_rewrite": "before the merge, a human commit changed lines an agent commit of the pull request wrote",
	"review_change": "before the merge, a reviewer commented and a later commit changed the commented lines",
	"ci_fix":        "before the merge, a check failed on the agent's pull request and a human commit made it pass",
	"revert":        "after the merge, the agent's pull request was reverted",
	"fix":           "after the merge, a fix changed lines of the agent's pull request",
}

func nullable(values []string) map[string]any {
	enum := make([]any, 0, len(values)+1)
	for _, v := range values {
		enum = append(enum, v)
	}
	return map[string]any{"type": []string{"string", "null"}, "enum": append(enum, nil)}
}

func labelSchema(withIntent bool) map[string]any {
	props := map[string]any{
		"reason":         map[string]any{"type": "string", "description": "One or two short sentences."},
		"wentWrong":      nullable(WentWrongs),
		"wentWrongLabel": map[string]any{"type": []string{"string", "null"}, "description": "Only with wentWrong other: at most 6 words."},
		"prevention":     nullable(Preventions),
		"confidence":     map[string]any{"type": "number", "minimum": 0, "maximum": 1},
	}
	required := []string{"reason", "wentWrong", "prevention", "confidence"}
	if withIntent {
		props["intent"] = map[string]any{"type": "string", "enum": Intents}
		required = []string{"reason", "intent", "confidence"}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func taskSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"taskType":   map[string]any{"type": "string", "enum": TaskTypes},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		},
		"required": []string{"taskType", "confidence"},
	}
}

// system returns the system prompt with the team's examples, newest first, at most 20.
func system(examples []Example) string {
	var b strings.Builder
	b.WriteString(systemPrompt)
	shown := 0
	for _, ex := range examples {
		if shown == exampleLimit || !ex.Window.Sendable() {
			continue
		}
		if shown == 0 {
			b.WriteString("\n\nPeople of this team labelled these interventions. Follow how they label.")
		}
		shown++
		labels, _ := json.Marshal(Label{Intent: ex.Intent, WentWrong: ex.WentWrong, WentWrongLabel: ex.WentWrongLabel, Prevention: ex.Prevention, Confidence: 1})
		fmt.Fprintf(&b, "\n\n<example %d>\n%s\nLabels: %s\n</example %d>", shown, render(ex.Window, nil, exampleTurnLimit), labels, shown)
	}
	return b.String()
}

// user returns the message that shows one window.
func user(w Window, commits []Commit) string {
	var b strings.Builder
	if w.RuleIntent != "" {
		fmt.Fprintf(&b, ruleNote+"\n\n", w.RuleIntent)
	}
	b.WriteString(render(w, commits, turnLimit))
	return b.String()
}

func render(w Window, commits []Commit, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Signal: %s (%s)", w.Signal, strings.ReplaceAll(w.Phase, "_", " "))
	if meaning := signalMeaning[w.Signal]; meaning != "" {
		fmt.Fprintf(&b, ": %s", meaning)
	}
	b.WriteString(".\n")
	if w.Agent != "" {
		fmt.Fprintf(&b, "Agent: %s", w.Agent)
		if w.Model != "" {
			fmt.Fprintf(&b, ", model %s", w.Model)
		}
		b.WriteString(".\n")
	}
	if w.Before != nil {
		b.WriteString("\nThe agent's last turn before the intervention:\n")
		writeTurn(&b, w.Before, limit)
	}
	if w.Human != "" {
		fmt.Fprintf(&b, "\nWhat the human wrote:\n%s\n", clip(w.Human, 4000))
	} else if w.Phase == "in_session" {
		b.WriteString("\nWhat the human wrote: no text (not captured, or the human wrote nothing).\n")
	}
	if len(w.Files) > 0 {
		fmt.Fprintf(&b, "\nFiles the human edited or the denial named: %s\n", strings.Join(w.Files, ", "))
	}
	if w.After != nil {
		b.WriteString("\nThe agent's next turn:\n")
		writeTurn(&b, w.After, limit)
	}
	if len(commits) > 0 {
		b.WriteString("\nCommits:\n")
		budget := diffLimit
		for _, c := range commits {
			fmt.Fprintf(&b, "\ncommit %s\n%s\n", short(c.SHA), strings.TrimSpace(clip(c.Message, 2000)))
			diff := clip(c.Diff, budget)
			budget -= len(diff)
			if diff != "" {
				fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(diff, "\n"))
			}
		}
	} else if len(w.Commits) > 0 {
		fmt.Fprintf(&b, "\nCommits: %s (their messages and diffs are not available).\n", strings.Join(w.Commits, ", "))
	}
	return b.String()
}

func writeTurn(b *strings.Builder, t *Turn, limit int) {
	if t.Text != "" {
		fmt.Fprintf(b, "%s\n", clip(t.Text, limit))
	} else {
		b.WriteString("(no text)\n")
	}
	for _, tool := range t.Tools {
		fmt.Fprintf(b, "- tool %s", tool.Name)
		if tool.Status != "" {
			fmt.Fprintf(b, " (%s)", tool.Status)
		}
		if len(tool.Files) > 0 {
			fmt.Fprintf(b, ": %s", strings.Join(tool.Files, ", "))
		}
		b.WriteString("\n")
	}
}

// clip cuts s to at most limit bytes, on a rune boundary, and says that it did.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const mark = "\n[… cut]"
	if limit <= len(mark) {
		return ""
	}
	cut := limit - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
