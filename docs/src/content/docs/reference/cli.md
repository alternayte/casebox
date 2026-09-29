---
title: CLI
description: Every casebox command, its flags and what it does, generated from the CLI itself.
---

This page lists every `casebox` command with its flags. It is generated from the CLI's own help, so it matches the version you run: `casebox <command> --help` shows the same text.

## casebox api

Call a route under /api/v1 with this machine's login (or CASEBOX_SERVER and CASEBOX_TOKEN when set) and print
the answer as indented JSON. The API reference lists the routes. The agent skill reads Casebox with it.

```text
casebox api <GET|POST|PUT|DELETE> <path> [flags]
```

```text
      --data string   a JSON body for POST and PUT
```

## casebox cases

Approved cases

```text
casebox cases
```

## casebox cases export

Write each approved case (all of the workspace's, or the ids given) as a Harbor task (task format 1.4)
in <dir>/<workspace>-<case id>. Run it inside a checkout of the case's repository. The base tree comes
from git archive of the base commit, fetched from origin when missing. The recipe comes from casebox.yml,
and must be the recipe the case was validated with. Cases that span other repositories and steering
cases (decided by trace assertions) are skipped.
The oracle and the patches are blobs only a worker token may read: set CASEBOX_WORKER_TOKEN.

```text
casebox cases export --format harbor --out <dir> [ids…] [flags]
```

```text
      --format string      the task format: harbor (default "harbor")
      --out string         the directory the tasks go in
      --workspace string   the workspace (default: casebox.yml's)
```

## casebox ci

Run harness CI in a GitHub Action.
On a pull request that changes harness files, the server runs the smoke suite with the pull request's harness.
It compares the result with the cached baseline of the default branch and posts it as a comment on the pull request.
With --baseline (a scheduled run), the server scores the default branch's harness on the approved dev cases whose
score is missing or older than 7 days.
It reads casebox.yml, CASEBOX_SERVER and CASEBOX_TOKEN (a ci token), and GITHUB_EVENT_PATH, GITHUB_REPOSITORY and
GITHUB_SERVER_URL. --worker inline runs this CI run's jobs on this machine's Docker, with the model keys of this
environment. The exit code is 0, or 1 with --fail-on regression when the smoke run found a regression, or 2 when
the request failed.

```text
casebox ci [flags]
```

```text
      --baseline           score the default branch's harness (the scheduled run) instead of a pull request
      --event string       the GitHub event file (default: GITHUB_EVENT_PATH)
      --fail-on string     regression: exit 1 when the smoke run finds a regression (default "none")
      --timeout duration   how long to follow the CI run (default 1h30m0s)
      --worker string      inline: run this CI run's jobs on this machine; none: leave them to the organisation's workers (default "none")
```

## casebox compare

Run an evaluation: both sides on the same approved dev cases, in random interleaved order, with a verdict of
better, worse, equivalent or inconclusive and its interval. The candidate changes exactly one thing:
  model=<id>, agent=<name>[@version], harness=<git ref|none> or effort=<level>.
The baseline is evaluation.baseline of casebox.yml, with the harness at the repository's default branch;
--baseline agent=…,agent_version=…,model=…,effort=…,harness=…,max_turns=…,timeout_minutes=…,token_cap=… overrides it.
The prices of casebox.yml price the estimate. compare prints the estimate and asks before it starts; with --yes it
starts without asking when the estimate needs no confirmation. It then follows the checkpoints every 10 seconds;
Ctrl-C asks whether to cancel the evaluation.

```text
casebox compare --candidate <change> [flags]
```

```text
      --baseline string    override evaluation.baseline of casebox.yml: agent=…,agent_version=…,model=…,effort=…,harness=…
      --candidate string   the one change: model=<id>, agent=<name>[@version], harness=<ref|none> or effort=<level>
      --cap float          the most this evaluation may spend in USD (the organisation's cap per evaluation is the upper bound)
      --cases int          use at most this many approved dev cases (0: all)
      --delta float        the equivalence margin δ, as a share (0.05 is 5 points) (default 0.05)
      --purpose string     compare or harness_vs_none (default compare; harness_vs_none for harness=none)
      --repeats int        runs of each case on each side (default 3)
      --yes                start without asking when the estimate needs no confirmation
```

## casebox doctor

Show each capture source, agent coverage and the spool

```text
casebox doctor
```

## casebox down

Stop the local server, QueueBox and Postgres

```text
casebox down [flags]
```

```text
      --volumes   also delete the database
```

## casebox env

The workspace's environment recipe: draft, check, confirm

```text
casebox env
```

## casebox env check

Propose the recipe to the server and build it with this machine's sandbox provider (CASEBOX_SANDBOX, default
docker). Then copy the repository at HEAD into a sandbox, run every test command, and record the result on the server.

```text
casebox env check
```

## casebox env confirm

Confirm the recipe whose check passed. Cases are mined only in a workspace with a confirmed recipe.

```text
casebox env confirm
```

## casebox env draft

Draft the environment block of casebox.yml from the devcontainer, Dockerfile, compose files, CI workflows
and the languages in this repository. It prints the block; paste it into .casebox/casebox.yml and adjust it.

```text
casebox env draft
```

## casebox erase

Erase every token a person had in every period: their sessions, trace events and telemetry are deleted,
and their correction text becomes unreadable. Name the identity with its kind, such as email:alice@example.com,
github:alice or jira:alice; the server also erases the identities its roster knows for the same person.
Backups taken before the erasure keep the old keys until they age out.

```text
casebox erase --identity <kind:value> [flags]
```

```text
      --identity string   the identity to erase, such as email:alice@example.com
```

## casebox import

Import the Claude Code and Codex sessions of the last days that ran in this repository, and upload them.
Then run steering detection, wait while a worker classifies the interventions, and print the report's headline.
Running it again imports only what is new.

```text
casebox import [flags]
```

```text
      --days int        how many days of history to import (default 30)
      --wait duration   how long to wait for steering classification (0 skips waiting) (default 10m0s)
```

## casebox init

Connect to the server, choose the team's prompt mode, create the workspace, write .casebox/casebox.yml,
and install capture hooks for the agents on this machine. Every step is safe to run again.

```text
casebox init [flags]
```

```text
      --github-issues          track GitHub Issues as work items
      --jira-project strings   a Jira project key, such as PAY; repeat for more
      --jira-url string        the Jira Data Center URL, such as https://jira.example.com
      --prompt-mode string     off, redacted or full; without it, init asks
      --server string          the Casebox server (default: casebox.yml, then http://localhost:8080)
      --workspace string       the workspace name (default: the repository name)
```

## casebox join

Log in, install capture hooks for the agents on this machine, and import your recent history.

```text
casebox join [flags]
```

```text
      --days int   how many days of history to import (default 30)
```

## casebox link

Link the sessions in this repository to a work item, such as PAY-123 or #42, until you link another one or run casebox link --clear.
An explicit link is the strongest signal: sessions linked this way can become cases.

```text
casebox link <work item> [flags]
```

```text
      --clear   remove the link
```

## casebox mine

Queue a mining job for each repository of the workspace in casebox.yml. A worker mines candidate cases
from its mirror. A worker with a sandbox provider validates them. A worker with an analysis model drafts
their instructions. With --wait, it polls until the case counts stop changing (at most 30 minutes).

```text
casebox mine [flags]
```

```text
      --wait   poll until the case counts stop changing (at most 30 minutes)
```

## casebox pause

Stop capture on this machine at once, for every repository. No reason is needed, and nobody is told.

```text
casebox pause
```

## casebox propose

Run the proposer. For each open pattern (or the one --pattern names) it drafts 3 to 5 small harness edits with
the analysis model. It scores them on a dev batch against the cached baseline and sends the best to the held-out gate.
A passing gate opens a pull request; a person merges it.
--scheduled is the Action's weekly run: every open pattern of the workspace, and the harness diet once a quarter.
The baseline, prices and repeats come from casebox.yml. It uses CASEBOX_SERVER and CASEBOX_TOKEN (a ci token) when
set, else this machine's login (a Member). The search budget is --budget-runs agent runs per pattern, and the
proposer spends at most its share of the monthly budget. --worker inline runs the jobs here, and waits for them.

```text
casebox propose (--pattern <id> | --scheduled) [flags]
```

```text
      --budget-runs int    the search budget per pattern, in agent runs (default 40)
      --pattern string     the pattern to propose for now
      --scheduled          every open pattern of the workspace, and the diet once a quarter (the Action's weekly run)
      --timeout duration   how long an inline worker runs (default 5h0m0s)
      --worker string      inline: run the proposer's jobs on this machine and wait for them (default "none")
```

## casebox resume

Start capture again on this machine

```text
casebox resume
```

## casebox review

Walk the review queue (validated cases with a drafted instruction). For each case it prints the source,
the tests that decide it, the instruction and the interfaces. Then it asks: [a]pprove, [e]dit the instruction
in $EDITOR, [r]eject with a reason, [s]kip or [q]uit. --approve-all approves the whole queue and lists the
cases the server refused, with the reason.

```text
casebox review [flags]
```

```text
      --approve-all        approve every case in the queue
      --workspace string   review only this workspace's cases (default: all)
```

## casebox skill

Install Casebox's agent skill

```text
casebox skill
```

## casebox skill install

Write Casebox's agent skill where the agent reads user-level skills or rules: ~/.claude/skills/casebox/SKILL.md
for Claude Code, ~/.cursor/rules/casebox.mdc for Cursor. Running it again replaces the file with this version's.

```text
casebox skill install --agent claude-code|cursor [flags]
```

```text
      --agent string   claude-code or cursor
```

## casebox steering

Steering classification

```text
casebox steering
```

## casebox steering check

Run the steering classifier with this machine's analysis model on the labelled set shipped in the CLI
(synthetic transcripts) and report how often it agrees with the labels, step by step.
With --team, compare the model's labels with your team's relabels on the server instead; no model is called.

```text
casebox steering check [flags]
```

```text
      --concurrency int   model calls at once (default 4)
      --limit int         classify only the first n labelled interventions (default: all)
      --team              compare with the team's relabels on the server
```

## casebox token

Issue tokens for workers, ingest and harness CI (admin)

```text
casebox token
```

## casebox token create

Issue a token for this organisation. A worker token runs casebox worker; an ingest token sends OTel data;
a ci token runs casebox ci in a GitHub Action (CASEBOX_TOKEN) and its inline worker. The server keeps only its
hash, so the secret is shown once: put it in a secret store now.

```text
casebox token create --kind worker|ingest|ci --name <name> [flags]
```

```text
      --kind string   worker, ingest or ci
      --name string   what the token is for
```

## casebox up

Start a local Casebox server, QueueBox and Postgres in Docker, and wait until they are healthy.
Running it again keeps the data and the admin password.

```text
casebox up [flags]
```

```text
      --demo           load a synthetic team to see every page before connecting anything
      --image string   the server image to run instead of the one that matches this CLI
      --port int       the port of the web UI and API (default 8080)
```

## casebox worker

Run a worker: it leases jobs from the server and runs them on this host.
It reads its worker token from CASEBOX_WORKER_TOKEN, and a GitHub token for cloning from GITHUB_TOKEN.
Model API keys stay in this host's environment; the server never sees them.
Sandboxes come from CASEBOX_SANDBOX (docker, kiln or daytona; default docker), at most CASEBOX_SANDBOX_CONCURRENCY
at once (default 2); environments, case validation and evaluation runs need one.
Evaluation runs also need a model key for the agents they run (ANTHROPIC_API_KEY, OPENAI_API_KEY or CURSOR_API_KEY).
Steering classification and case instructions need an analysis model: CASEBOX_ANALYSIS_PROVIDER (anthropic or openai, any
OpenAI-compatible API), CASEBOX_ANALYSIS_MODEL, and optionally CASEBOX_ANALYSIS_BASE_URL and CASEBOX_ANALYSIS_API_KEY
(default ANTHROPIC_API_KEY or OPENAI_API_KEY).

```text
casebox worker [flags]
```

```text
      --id string       the worker ID (default: host name and process ID)
      --server string   the Casebox server (default: CASEBOX_SERVER, then this machine's credentials)
```

