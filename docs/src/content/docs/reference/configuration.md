---
title: casebox.yml
description: Every key of .casebox/casebox.yml, with its default, and the JSON Schema that editors use for completion.
---

This page describes `.casebox/casebox.yml`, the file that enrols a repository in Casebox. It lives in the repository and you review it like code. Privacy settings (the prompt mode, k and the pseudonym period) live on the server, where only Admins change them.

The file has a [JSON Schema](/schema/casebox.schema.json). `casebox init` writes this first line, so editors with the YAML language server complete and check every key:

```yaml
# yaml-language-server: $schema=https://casebox-docs.pages.dev/schema/casebox.schema.json
```

Every example on this page is checked against the schema in CI.

## What casebox init writes

```yaml title="casebox.yml"
version: 1
server: http://localhost:8080
workspace: payments
repos:
  - github.com/acme/payments-api
```

## A single repository with Jira

```yaml title="casebox.yml"
version: 1
server: https://casebox.example.com
workspace: payments
repos:
  - github.com/acme/payments-api
work_items:
  jira: { url: https://jira.example.com, projects: [PAY] }
```

## Several repositories in one workspace

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
  globs: [AGENTS.md, CLAUDE.md, ".cursor/rules/**", ".claude/skills/**", ".cursor/mcp.json"]
capture:
  redact: ["ACME-[0-9]{6}"]
```

## Keys

| Key | Default | Meaning |
| --- | --- | --- |
| `version` | required | Always `1`. |
| `server` | none | The Casebox server. `CASEBOX_SERVER` overrides it. |
| `workspace` | the repository name | A workspace groups repositories whose corrections form patterns together. |
| `repos` | this repository | The workspace's repositories, as `host/owner/name`. |
| `work_items.jira` | none | `url` of Jira Data Center and the `projects` whose keys link work. |
| `work_items.github_issues` | `false` | Track GitHub Issues as work items. |
| `harness.globs` | `AGENTS.md`, `CLAUDE.md`, `.cursor/rules/**`, `.claude/skills/**`, `.agents/skills/**`, `.mcp.json`, `.cursor/mcp.json` | The harness files: what Casebox reads when it drafts a proposal, and what a proposal may change. A glob matches the whole repo-relative path. |
| `capture.redact` | none | More regular expressions to redact before anything leaves a machine. |
