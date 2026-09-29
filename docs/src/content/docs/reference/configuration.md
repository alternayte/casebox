---
title: casebox.yml
description: Every key of .casebox/casebox.yml, with its default, and the JSON Schema that editors use for completion.
---

This page describes `.casebox/casebox.yml`, the file that enrols a repository in Casebox. It lives in the repository and you review it like code. Privacy settings (the prompt mode, k and the pseudonym period) and budgets live on the server, where only Admins change them.

The file has a [JSON Schema](/schema/casebox.schema.json). `casebox init` writes this first line, so editors with the YAML language server complete and check every key:

```yaml
# yaml-language-server: $schema=https://casebox-docs.pages.dev/schema/casebox.schema.json
```

Every example on this page is checked against the schema in CI.

## A single repository

```yaml title="casebox.yml"
version: 1
server: https://casebox.example.com
workspace: payments
repos:
  - github.com/acme/payments-api
work_items:
  jira: { url: https://jira.example.com, projects: [PAY] }
environment:
  image: mcr.microsoft.com/dotnet/sdk:10.0
  install: ["dotnet restore"]
  test:
    - command: dotnet test --logger trx --results-directory /results
      results: trx
  services:
    postgres: { image: "postgres:16", env: { POSTGRES_PASSWORD: test } }
evaluation:
  baseline: { agent: claude-code, agent_version: 2.1.284, model: claude-sonnet-5-20260801 }
prices:
  claude-sonnet-5-20260801: { input: 3, output: 15, cache_read: 0.3 }
```

## Several repositories and a shared harness

```yaml title="casebox.yml"
version: 1
server: https://casebox.example.com
workspace: checkout
repos:
  - github.com/acme/checkout-api
  - github.com/acme/checkout-web
work_items:
  github_issues: true
harness:
  globs: [AGENTS.md, CLAUDE.md, ".claude/skills/**", ".mcp.json"]
  shared: github.com/acme/eng-harness
environment:
  image: golang:1.26
  lockfiles: [go.sum, web/bun.lock]
  test:
    - command: go test -json ./...
      results: go-test-json
      timeout: 15m
  links: [go.work]
cases: { window: 120, max_source_files: 8 }
suites:
  smoke: { size: 12, repeats: 1 }
  full: { repeats: 5 }
evaluation:
  baseline: { agent: codex, agent_version: 0.159.0, model: gpt-6-sol-2026-06-01, effort: high }
prices:
  gpt-6-sol-2026-06-01: { input: 1.25, output: 10, cache_read: 0.125 }
```

## A shared harness repository

A shared harness repository has no workspace. Its pull requests run harness CI in every workspace that names it in `harness.shared`.

```yaml title="casebox.yml"
version: 1
server: https://casebox.example.com
harness:
  globs: [AGENTS.md, CLAUDE.md, ".claude/skills/**"]
evaluation:
  baseline: { agent: claude-code, agent_version: 2.1.284, model: claude-sonnet-5-20260801 }
prices:
  claude-sonnet-5-20260801: { input: 3, output: 15, cache_read: 0.3 }
```

## Keys

| Key | Default | Meaning |
| --- | --- | --- |
| `version` | required | Always `1`. |
| `server` | none | The Casebox server. `CASEBOX_SERVER` overrides it. |
| `workspace` | the repository name | A workspace groups repositories with one environment recipe. |
| `repos` | this repository | The workspace's repositories, as `host/owner/name`. |
| `work_items.jira` | none | `url` of Jira Data Center and the `projects` whose keys link work. |
| `work_items.github_issues` | `false` | Track GitHub Issues as work items. |
| `harness.globs` | `AGENTS.md`, `CLAUDE.md`, `.cursor/rules/**`, `.claude/skills/**`, `.agents/skills/**`, `.mcp.json` | The harness files. A glob matches the whole repo-relative path. |
| `harness.shared` | none | A shared harness repository. Its files go into each agent's user-level configuration in a replay. |
| `capture.redact` | none | More regular expressions to redact before anything leaves a machine. |
| `environment` | none | The recipe: `image`, `install`, `lockfiles`, `test` (each with `command`, `results` and `timeout`), `services` and `links`. `casebox init` drafts it and a person confirms it. |
| `cases.window` | `183` | Days back from today that mining looks. |
| `cases.max_source_files` | `12` | The most non-test files a mined change may touch. |
| `cases.test_globs` | common test paths | What counts as a test file. |
| `evaluation.baseline` | none | The agent, its version, the model, and optional `effort`, `max_turns`, `timeout_minutes`, `token_cap` and `command`. |
| `suites.smoke` | `size: 10`, `repeats: 1` | The pull request smoke run of harness CI. |
| `suites.full.repeats` | `3` | Repeats of the nightly baseline and of the proposer's gate. |
| `prices` | none | USD per million tokens by model: `input`, `output`, and optional `cache_read` and `cache_write`. A model without a price is refused. |

Test result formats are `go-test-json`, `trx` (.NET) and `junit` (Java, JavaScript with `jest-junit`, Python with `pytest --junitxml`). A test command without a format counts by its exit code only, so it never proves that a change did the work.
