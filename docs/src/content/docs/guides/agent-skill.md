---
title: Install the agent skill
description: Let Claude Code or Cursor read the steering report, explain a verdict and open a case for you.
---

This guide installs Casebox's agent skill. With it, your coding agent can answer "what does my team get corrected for?" or "why is this evaluation inconclusive?" from your server's data.

```bash
casebox skill install --agent claude-code   # writes ~/.claude/skills/casebox/SKILL.md
casebox skill install --agent cursor        # writes ~/.cursor/rules/casebox.mdc
```

The skill reads Casebox with `casebox api GET <path>`, using your login from `casebox init` or `casebox join`. It explains verdicts with their interval and sample size, labels model-generated text as such, and never tries to identify a person.
