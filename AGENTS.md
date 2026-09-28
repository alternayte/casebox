# Casebox

## What this is

Casebox measures how often people must steer their coding agents, turns that history into replayable cases, and tests harness changes on those cases before a human merges them.
It is a Go CLI and worker, a .NET server on Deedbox and Postgres, QueueBox for every outbox and inbox, and a React UI served by the server; work follows the step list and decisions in `PROGRESS.md`, and the design doc is local only.

## Run

- `docker compose -f deploy/compose.yaml up --build` starts Postgres, QueueBox and the server on port 8080.
- `cd web && bun run dev` serves the UI with hot reload.

## Test

- `just check` is the gate: repo checks, then the CLI, the web UI and the server.
- `just cli`, `just web` and `just server` run one part.
- Server tests need Docker; Testcontainers starts Postgres 16 and the pinned QueueBox image.

## Stack rules

- The server targets net10.0 only; warnings are errors.
- The CLI builds without CGO, so it stays one static binary on Linux, macOS and Windows.
- The web UI builds into the server's static files; there is no separate web deployable.
- Business decisions are Deedbox events; high-volume telemetry and blobs are plain Postgres tables.
- Queries are Dapper over explicit SQL.
- Side effects leave through the QueueBox outbox; webhooks and poll results enter through the QueueBox inbox.
- Never persist a raw identity; people exist only as pseudonymous tokens.
- The server never runs case code and never holds model API keys; workers do both.
- A gap in Deedbox, QueueBox or Kiln is fixed in that repository, not worked around here.
- Never weaken, skip or delete a test to make it pass.

## Domain words

- work item: a Jira or GitHub issue; every report groups by it.
- workspace: one or more repos with one environment recipe.
- session: one agent run, captured live or imported.
- steering event: a human intervention in or after a session.
- correction: a steering event that fixes the agent's work; only corrections count as steering.
- harness: the repo's agent instruction files, skills and MCP config, plus the agent, model and settings.
- harness version: the hash of the harness files plus the agent and model identifiers.
- case: a replayable task from history, rewound to its base commit.
- oracle: what decides a case: fail-to-pass and pass-to-pass tests, trace assertions and narrow judge questions.
- suite: a named set of approved cases, split into a dev set and a held-out set.
- evaluation: a controlled comparison of a baseline and a candidate harness on a suite.
- verdict: better, worse, equivalent or inconclusive, with sample size and interval.
- pattern: a cluster of corrections or failures that share a cause.
- proposal: a small harness edit, delivered as a pull request after it passes the gate.
- token: the pseudonymous ID of a person, stable within a period.
- k: the minimum number of distinct people behind any shown group.
- gate: a step in the build prompt where work stops for human review.
