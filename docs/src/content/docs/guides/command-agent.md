---
title: Add an agent with the command template
description: Evaluate any headless coding agent, such as pi or OpenCode, with the command agent.
---

This guide evaluates an agent that Casebox has no adapter for. The `command` agent runs any headless CLI from a template.

```yaml
evaluation:
  baseline:
    agent: command
    model: <model id>
    command:
      template: opencode run --model {model} --file {instruction_file}
      log_glob: .local/share/opencode/sessions/*.jsonl
      log_format: none
```

- `{instruction_file}` becomes the path of the task's instruction, and `{model}` the model. Both are shell-quoted.
- `log_glob` finds the session log after the run, relative to the sandbox user's home unless absolute.
- `log_format` names the parser: `claude-code`, `codex`, `cursor-cli` or `none`. With `none`, Casebox records the diff and the tests but no token usage, so cost comes from the price table only.

The agent's install steps go into the environment recipe, pinned to a version. The worker passes the model keys it holds into the agent's sandbox and nowhere else.

A comparison changes one thing: a command agent can compare models or harnesses, but not switch to another agent: its template changes too, and that is a second change.
