---
title: A sandbox provider down
description: Workers cannot start sandboxes, so validation and evaluation runs fail.
---

## Symptom

- Evaluation runs end as "failed to run", and case validation fails with infrastructure errors.

## What shows it

- `casebox_runs_total{outcome="failed"}` rises against `{outcome="completed"}`.
- `casebox_sandbox_start` (and the worker's `casebox.worker.sandbox.start`) grows or stops.
- The worker's log shows `CBX050`, `CBX051` or `CBX052`.

## Steps

1. Docker: run `docker info` on the worker's host. The daemon must run Linux containers (`CBX051`). Free disk space: images and snapshots of old cases add up; `docker image prune` removes unused ones.
2. Kiln or Daytona: check the provider's status and the credentials in the worker's environment. The worker's first log lines show whether the provider answered.
3. Agent runs need network control that only the Docker provider has today (`CBX052`). Run evaluation workers on Docker.
4. When the provider is back, run the failed step again. An evaluation whose round had failed runs counts them as failed runs; start a new evaluation for a clean result.
