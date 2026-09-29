---
title: Use Kiln or Daytona
description: Run sandboxes on Kiln microVMs or on Daytona instead of the local Docker daemon.
---

This guide switches a worker's sandbox provider from Docker to Kiln or Daytona. Docker is the default for a trial; Kiln isolates untrusted code better, because each sandbox is a microVM.

| Provider | Set | Notes |
| --- | --- | --- |
| Docker | nothing, or `CASEBOX_SANDBOX=docker` | Needs a daemon that runs Linux containers. Supports every job. |
| Kiln | `CASEBOX_SANDBOX=kiln`, `KILN_URL`, `KILN_API_KEY` | Snapshots and forks are native. |
| Daytona | `CASEBOX_SANDBOX=daytona`, `DAYTONA_API_KEY`, optional `DAYTONA_API_URL` | Daytona's cloud by default. Services must be public images. |

Start the worker with the variables set:

```bash
CASEBOX_SANDBOX=kiln KILN_URL=<url> KILN_API_KEY=<key> casebox worker
```

Its first lines say which provider answered. `CASEBOX_SANDBOX_CONCURRENCY` caps the sandboxes it runs at once (default 2).

Limits in 0.1.0:

- Agent runs need egress limited to the model API and Casebox's registry mirror. Kiln and Daytona cannot enforce that yet, so they refuse agent runs (`CBX052`). They run environment builds and case validation.
- Kiln refuses sidecar services and open network until its open issues ship.
