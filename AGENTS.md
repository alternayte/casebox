# Casebox

## What this is

Casebox measures how often people must steer their coding agents, groups the corrections into patterns, and proposes small repository changes that a person approves or rejects.
It is a Go CLI and worker, a .NET server on Deedbox and Postgres, QueueBox for the poll inbox, and a React UI served by the server; work follows the step list and decisions in `PROGRESS.md`, and the design doc is local only.

## Run

- `CASEBOX_ADMIN_PASSWORD=pw CASEBOX_POLL_TOKEN=po CASEBOX_QUEUEBOX_ADMIN_TOKEN=qa docker compose -f deploy/compose.yaml up --build` starts Postgres, QueueBox and the server on port 8080; the three secrets are required, and casebox up generates them.
- `cd cli && go run ./cmd/casebox --help` runs the CLI; set CASEBOX_HOME to a scratch directory to keep its files out of ~/.casebox.
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
- Webhooks and poll results enter through the QueueBox inbox; the server writes nothing to a git host.
- Never persist a raw identity; people exist only as pseudonymous tokens.
- The server never holds model API keys; workers run the analysis model, and the apply command runs the person's own agent on their machine.
- A gap in Deedbox or QueueBox is fixed in that repository, not worked around here.
- Never weaken, skip or delete a test to make it pass.

## Domain words

- work item: a Jira or GitHub issue; every report groups by it.
- workspace: one or more repos whose corrections are grouped and proposed on together.
- session: one agent run, captured live or imported.
- steering event: a human intervention in or after a session.
- correction: a steering event that fixes the agent's work; only corrections count as steering.
- harness: the repo's agent instruction files, skills and MCP config, plus the agent, model and settings.
- harness version: the hash of the harness files plus the agent and model identifiers.
- pattern: a cluster of corrections or failures that share a cause.
- proposal: a small change to a workspace's repository files (a harness edit, a skill, an MCP entry, or a code note that the person's agent carries out), drafted from a pattern and approved or rejected by a person. Avoid: suggestion, recommendation.
- token: the pseudonymous ID of a person, stable within a period.
- identity mark: an identity the CLI wraps as ⟦cbx:kind:value⟧; only the server turns it into a token.
- k: the minimum number of distinct people behind any shown group.
- gate: a step in the build prompt where work stops for human review.
