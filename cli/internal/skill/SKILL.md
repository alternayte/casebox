---
name: casebox
description: Read a team's Casebox data from a coding agent - the steering report, its patterns and the proposals drafted from them - and explain it honestly. Use when the user asks what their agents get corrected for, what Casebox proposes and why, or whether an applied change helped.
---

# Casebox

Casebox measures how often people correct their coding agents, groups the corrections into patterns, and proposes small repository changes (an instruction, a skill, an MCP server, or a code note) that a person approves or rejects. Docs: https://casebox-docs.pages.dev/llms-full.txt

## Read the data

Run `casebox api GET <path>` in a repository enrolled with `casebox init` or `casebox join`. It prints the server's JSON.

| Question | Route |
| --- | --- |
| What do people correct, and why? | `/api/v1/steering/report?from=<yyyy-mm-dd>&to=<yyyy-mm-dd>` |
| Which patterns exist? | `/api/v1/patterns`, then `/api/v1/patterns/<id>` for its quotes |
| What does Casebox propose? | `/api/v1/proposals`, then `/api/v1/proposals/<id>` for its evidence, its change and its outcome |

`casebox proposals` and `casebox proposals show <id>` print the same in plain text.

## Explain a proposal

- Give its kind, its title, the corrections behind it (count, people, and a quote), and the change itself.
- An open proposal waits for the person: they run `casebox proposals approve <id>` or `casebox proposals reject <id> --reason "…"`. Do not approve or reject for them.
- An approved proposal lands with `casebox apply <id>` (private: its own file in `.git/info/exclude`) or `casebox apply <id> --commit` (the shared files). A code note starts the person's agent with its prompt.
- An outcome compares the pattern's corrections per 100 sessions in the 30 days before and after the apply. It is observational: say that other changes in the same weeks affect it too.

## Rules

- Steering numbers and outcomes are observational. Never describe a difference between groups, or before and after, as proven cause.
- Titles, summaries and rationales of patterns and proposals are model-generated. Say so when you quote them.
- Never try to find out who a person is. Casebox shows groups of at least k people and quotes without authors; `[person]` marks a person. Never rank or compare people. While one person is the whole organisation, the report shows everything, and says so.
- Every error has a CBX code; its page states the fix: https://casebox-docs.pages.dev/reference/errors/
