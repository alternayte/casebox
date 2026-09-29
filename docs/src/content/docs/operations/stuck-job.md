---
title: A stuck job
description: A job stays queued or leased and the work it stands for does not finish.
---

This runbook helps you find a job that does not finish and get it running again.

## Symptom

- Classification does not move, or no proposal appears for a pattern.

## What shows it

- `casebox_jobs_queued{kind}` stays above 0 and `casebox_jobs_oldest{kind}` grows.
- `casebox_jobs_lease_expiries_total{kind}` increases: a worker takes the job and stops before it answers.

## Steps

1. Find the job and its last error:

   ```sql
   SELECT id, kind, status, attempts, max_attempts, lease_owner, last_error, available_at
   FROM casebox.jobs WHERE status IN ('queued', 'leased') ORDER BY available_at LIMIT 20;
   ```

2. A queued job with no worker for its kind: start a worker that can run it (see [A worker offline](/operations/worker-offline/)). A worker leases only the kinds it can run: `steering.classify`, `pattern.cluster` and `proposal.draft` need an analysis model.
3. A job whose lease expires again and again: read the worker's log for the job ID. A worker that runs out of memory or time stops before it answers. Give it more resources, or lower `CASEBOX_SANDBOX_CONCURRENCY`.
4. A job that fails with the same error: the error has a CBX code when it is a setup problem. Fix what its page says. The job runs again after its back-off, until its attempts are used up.
5. A job that failed for good does not run again by itself. The step that made it makes a new job when its input changes: a new import, or new corrections for a pattern.
