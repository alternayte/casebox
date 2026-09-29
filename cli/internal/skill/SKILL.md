---
name: casebox
description: Read a team's Casebox data from a coding agent - the steering report, an evaluation's verdict, a case, a pattern or a proposal - and explain it honestly. Use when the user asks what their agents get corrected for, whether a harness change helped, why a verdict came out as it did, or what a case or proposal contains.
---

# Casebox

Casebox measures how often people correct their coding agents, turns that history into replayable cases, and tests harness changes on them. Docs: https://casebox-docs.pages.dev/llms-full.txt

## Read the data

Run `casebox api GET <path>` in a repository enrolled with `casebox init` or `casebox join`. It prints the server's JSON.

| Question | Route |
| --- | --- |
| What do people correct, and why? | `/api/v1/steering/report?from=<yyyy-mm-dd>&to=<yyyy-mm-dd>` |
| What patterns and proposals exist? | `/api/v1/patterns`, `/api/v1/patterns/<id>`, `/api/v1/proposals/<id>` |
| Which evaluations ran? | `/api/v1/evaluations`, then `/api/v1/evaluations/<id>` and `/api/v1/evaluations/<id>/cases` |
| What does a case ask, and what decides it? | `/api/v1/cases/<id>`, `/api/v1/cases/<id>/oracle` |

## Explain a verdict

- A verdict is better, worse, equivalent or inconclusive. Always give Δ (candidate minus baseline pass rate), its interval, the interval's level, and the number of cases and runs.
- Better: the whole interval is above 0. Worse: the whole interval is below 0. Equivalent: the whole interval lies within ±δ. Anything else is inconclusive.
- `reason: no_difference` means the interval straddles 0 within ±2δ: any real difference is small. `reason: smoke` is a harness CI smoke run, which never claims better. `reason: budget` means the budget ran out.
- `estimate.detectableEffect` is the smallest difference this size detects with power 0.8. A smaller real difference is likely to come out inconclusive: say so instead of calling it "no effect".
- In harness versus no harness, the candidate is no harness: "worse" means the harness earns its tokens.

## Rules

- Steering numbers are observational. Never describe a difference between groups as its cause. Verdicts are controlled comparisons; say which one you report.
- Titles, summaries and rationales of patterns and proposals are model-generated. Say so when you quote them.
- Never try to find out who a person is. Casebox shows groups of at least k people and quotes without authors; `[person]` marks a person. Never rank or compare people.
- Every error has a CBX code; its page states the fix: https://casebox-docs.pages.dev/reference/errors/
