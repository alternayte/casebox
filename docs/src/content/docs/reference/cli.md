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

## casebox apply

Write an approved proposal into this working tree.
Without --commit it stays private. New instruction bullets become their own Cursor rule, and a new skill,
rule or MCP config is written as itself. Every file goes into .git/info/exclude: git status stays clean,
and your team sees nothing. With --commit it edits the shared files for you to commit.
A code note starts your agent (the Cursor CLI, or Claude Code; --agent picks) with the note's prompt; you
steer it and review its diff like any other.

```text
casebox apply <id> [flags]
```

```text
      --agent string   the agent that carries out a code note: cursor or claude (default: the one installed)
      --commit         edit the shared files for your team instead of writing a private file
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

Import the Claude Code, Codex and Cursor CLI sessions of the last days that ran in this repository, and upload them.
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
      --ado-collection string   the Azure DevOps Server collection URL, such as https://ado.example.com/tfs/DefaultCollection (default: from the remote)
      --github-issues           track GitHub Issues as work items
      --jira-project strings    a Jira project key, such as PAY; repeat for more
      --jira-url string         the Jira Data Center URL, such as https://jira.example.com
      --prompt-mode string      off, redacted or full; without it, init asks
      --server string           the Casebox server (default: casebox.yml, then http://localhost:8080)
      --workspace string        the workspace name (default: the repository name)
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

## casebox pause

Stop capture on this machine at once, for every repository. No reason is needed, and nobody is told.

```text
casebox pause
```

## casebox proposals

List the changes Casebox proposes for this workspace: open ones wait for you to approve or reject them,
approved ones wait for casebox apply. --all also lists rejected and applied ones.

```text
casebox proposals [flags]
```

```text
      --all   also list rejected and applied proposals
```

## casebox proposals approve

Approve a proposal; casebox apply <id> then writes it

```text
casebox proposals approve <id>
```

## casebox proposals reject

Reject a proposal; the reason keeps the change from coming back for 90 days

```text
casebox proposals reject <id> --reason <why> [flags]
```

```text
      --reason string   why, in one line
```

## casebox proposals show

Show a proposal: why, the corrections behind it, and the change

```text
casebox proposals show <id>
```

## casebox resume

Start capture again on this machine

```text
casebox resume
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

## casebox uninstall

Remove the capture hooks casebox init and casebox join wrote for Claude Code, Codex and the Cursor CLI, and the
native telemetry settings that send Claude Code's and Codex's telemetry to the server. Every other hook and
setting stays. The server keeps what it received; ~/.casebox keeps this machine's login and spool, and you can
delete that directory and the casebox binary afterwards.

```text
casebox uninstall
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
It reads its worker token from CASEBOX_WORKER_TOKEN, and the tokens for cloning from GITHUB_TOKEN and, for Azure DevOps
Server, AZURE_DEVOPS_TOKEN (a personal access token with Code (Read)).
Model API keys stay in this host's environment; the server never sees them.
Steering classification, pattern splits and proposal drafts need an analysis model: CASEBOX_ANALYSIS_PROVIDER (anthropic, openai for
any OpenAI-compatible API, or cursor-agent), CASEBOX_ANALYSIS_MODEL, and optionally CASEBOX_ANALYSIS_BASE_URL and
CASEBOX_ANALYSIS_API_KEY (default ANTHROPIC_API_KEY or OPENAI_API_KEY). cursor-agent uses this machine's Cursor CLI
login and needs no key; its model defaults to auto.

```text
casebox worker [flags]
```

```text
      --id string       the worker ID (default: host name and process ID)
      --server string   the Casebox server (default: CASEBOX_SERVER, then this machine's credentials)
```

