---
title: Install the agent skill
description: Let Claude Code or Cursor read the steering report and explain Casebox's proposals for you.
---

This guide installs Casebox's agent skill. With it, your coding agent can answer "what does my team get corrected for?" or "what does Casebox propose, and why?" from your server's data.

```bash
casebox skill install --agent claude-code   # writes ~/.claude/skills/casebox/SKILL.md
casebox skill install --agent cursor        # writes ~/.cursor/rules/casebox.mdc
```

The skill reads Casebox with `casebox api GET <path>`, using your login from `casebox init` or `casebox join`. It explains each proposal with the corrections behind it, labels model-generated text as such, leaves every approval to you, and never tries to identify a person.
