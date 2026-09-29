---
title: A worker offline
description: No worker takes jobs, so imports are not classified, cases are not mined and evaluations do not run.
---

## Symptom

- The steering report says that no worker was seen in the last 10 minutes.
- `/healthz/ready` reports the workers check as degraded while jobs wait.

## What shows it

- `casebox_workers_seen` is 0.
- `casebox_jobs_queued` rises.

## Steps

1. Check the worker process. Kubernetes: `kubectl get pods -l app.kubernetes.io/component=worker` and the pod log. A host: the service that runs `casebox worker`.
2. Read the first lines of the worker's log. They say what the worker can run: the analysis model, the sandbox provider and the agents. A line that ends with a CBX code names the fix.
3. A worker that cannot reach the server logs `CBX012`. Check `CASEBOX_SERVER` and the network path to port 8080.
4. A worker whose token was revoked logs `CBX010`. Issue a new token (`casebox token create --kind worker --name <name>`) and update `CASEBOX_WORKER_TOKEN`.
5. When the worker runs but takes no job, compare its kinds with the queued jobs' kinds (see [A stuck job](../stuck-job/)).
