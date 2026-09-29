---
title: A stuck job
description: A job stays queued or leased and the work it stands for does not finish.
---

## Symptom

- Cases stay in "mined", an evaluation run does not finish, or classification does not move.

## What shows it

- `casebox_jobs_queued{kind}` stays above 0 and `casebox_jobs_oldest{kind}` grows.
- `casebox_jobs_lease_expiries_total{kind}` increases: a worker takes the job and stops before it answers.

## Steps

1. Find the job and its last error:

   ```sql
   SELECT id, kind, status, attempts, max_attempts, lease_owner, last_error, available_at
   FROM casebox.jobs WHERE status IN ('queued', 'leased') ORDER BY available_at LIMIT 20;
   ```

2. A queued job with no worker for its kind: start a worker that can run it (see [A worker offline](../worker-offline/)). A worker leases only the kinds it can run: `run` needs a model key, `steering.classify` and `propose.search` need an analysis model, sandbox jobs need a provider.
3. A job whose lease expires again and again: read the worker's log for the job ID. A worker that runs out of memory or time stops before it answers. Give it more resources, or lower `CASEBOX_SANDBOX_CONCURRENCY`.
4. A job that fails with the same error: the error has a CBX code when it is a setup problem. Fix what its page says. The job runs again after its back-off, until its attempts are used up.
5. A job that failed for good does not run again by itself. The step that made it (mining, a round of an evaluation) makes a new job when you run that step again.
