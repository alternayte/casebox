# Contributing to Casebox

Casebox is in early development. The first release is 0.1.0.

## Build and test

You need Go 1.26, the .NET 10 SDK, Bun, Docker and `just`.

```sh
just check
```

`just check` runs the repo checks, then builds and tests the CLI, the web UI and the server.
The server tests start Postgres 16 and the pinned QueueBox image with Testcontainers.
Run one part alone with `just cli`, `just web` or `just server`.

## Layout

| Directory | Holds | Licence |
| --- | --- | --- |
| `cli/` | The Go CLI and worker, one static binary | Apache-2.0 |
| `server/` | The .NET server | AGPL-3.0 |
| `web/` | The React UI, built into the server's static files | AGPL-3.0 |
| `action/` | The GitHub Action | Apache-2.0 |
| `deploy/` | The compose file and the Helm chart | Apache-2.0 |
| `ee/` | Reserved for commercial features; empty in 0.1.0 | Commercial |

## Rules

- Do not weaken, skip or delete a test to make it pass.
- Package by feature. Do not add a generic repository layer.
- Side effects leave through the QueueBox outbox, and incoming events enter through the QueueBox inbox. Do not write an outbox, an inbox or delivery retries by hand.
- A gap in Deedbox, QueueBox or Kiln is fixed in that repository, not worked around here.
- A lesson learned becomes a script in `checks/`.

## Contributor License Agreement

Every pull request needs a signed [CLA](CLA.md). The CLA bot comments on your first pull request with the sentence to post.
