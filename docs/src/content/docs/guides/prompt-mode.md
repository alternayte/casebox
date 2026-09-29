---
title: Choose a prompt mode
description: Decide what prompt text leaves your team's machines, and what steering analysis each choice allows.
---

This guide helps an Admin choose the team's prompt mode. `casebox init` asks for it, and there is no default: capture stays off until someone chooses (`CBX090`).

| Mode | Kept | Steering analysis |
| --- | --- | --- |
| `off` | Structure only: counts, timings, tool calls, edits | Counts only: follow-ups, interruptions, denials, human edits, review rounds |
| `redacted` | Prompt and response text after secrets are removed and identities are tokenized | Full: corrections are classified and clustered by their words |
| `full` | Also tool output and file contents shown to the agent | Full, with more context around each correction |

Most teams choose `redacted`. In every mode:

- Secrets are removed on the machine and again on the server, with the team's own rules from `capture.redact` added.
- Home paths become `~` and host names become `host`.
- Every identity becomes a pseudonymous token on the server, in memory, before anything is written.

To change the mode later, an Admin runs `casebox init` again or changes it in the settings. The change is recorded in the organisation's history. Claude Code and Codex log prompt text in their own telemetry only when told to; `casebox init` sets that to match the mode.
