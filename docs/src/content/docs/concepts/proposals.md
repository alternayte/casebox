---
title: Patterns and proposals
description: How recurring corrections become small changes to your repository that you approve or reject, and how Casebox checks them afterwards.
sidebar:
  order: 4
---

This page explains how Casebox turns corrections into changes that help your agent get it right the first time. You only approve or reject.

## Patterns

Corrections that share what went wrong, what prevents it next time and the area of the code form a group. The analysis model splits each group by cause. A part with at least 3 corrections from k people within 60 days becomes a pattern. While you are the only person in the organisation, 3 of your own corrections are enough. You can acknowledge a pattern, or dismiss it with a reason.

## Proposals

For each new pattern, a worker drafts one proposal of the kind that fits:

| Kind | What changes |
| --- | --- |
| Instruction edit | One to three bullets in `AGENTS.md`, `CLAUDE.md` or a `.cursor/rules` file |
| Skill | A new skill (`SKILL.md`) or a Cursor rule with a short procedure |
| MCP server | One entry in `.cursor/mcp.json` or `.mcp.json`; its secrets are `${VARIABLE}` placeholders |
| Code note | A change to the code that makes the mistake hard, such as one test command or a check, with a prompt for your agent |

Each proposal shows the corrections behind it, the reason, and the change as a diff. When no change to the repository prevents a pattern, such as an unclear ticket or a weak model, the pattern gets an advisory note instead.

At most three proposals wait for a person at a time, so you are never flooded. A rejection needs a one-line reason. The next draft reads it, and the same change does not come back for 90 days.

Casebox is not a harness and keeps no memory of its own. Your agent's memory is your repository's instruction files, skills and MCP config. Casebox only proposes small, reviewed changes to them, and a proposal never touches your personal agent settings.

## Applying a proposal

An approved proposal lands through your own machine, with `casebox apply <id>` in the repository:

- **Private**, the default: new instruction bullets become their own Cursor rule, `.cursor/rules/casebox-<id>.mdc`. A new skill, rule or MCP config is written as itself. Each file goes into `.git/info/exclude`, so `git status` stays clean and your team sees nothing. Use it to gather evidence before you ask your team.
- **Shared**, with `--commit`: the change edits the shared files, and you commit it and open a pull request as usual.
- **A code note** starts your agent (the Cursor CLI or Claude Code) with the note's prompt. You steer it and review its diff like any other change.

The server writes nothing to a git host.

## After it lands

30 days after a proposal is applied, Casebox compares its pattern's corrections per 100 sessions of the workspace in the 30 days before and after. A fall resolves the pattern. The comparison is observational: other changes in the same weeks affect it too.
