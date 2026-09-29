---
title: A stalled projection
description: A report, a page or a workflow stops moving because a projection or subscription no longer applies events.
---

## Symptom

- The steering report, the case catalog or an evaluation page shows old data.
- An evaluation or a proposal stays in one state and starts no new work.
- `/healthz/ready` on port 8081 answers unhealthy for a `deedbox` check.

## What shows it

- `deedbox_consumer_lag_seconds` rises for one consumer and does not fall.
- `deedbox_consumer_status` is not 0 for that consumer.
- `deedbox_consumer_stalls_total` increases.

## Steps

1. Read the state of every consumer:

   ```bash
   deedbox status --provider postgres --connection "$CASEBOX_DB"
   ```

   A stalled consumer shows the event it stopped on and the error.
2. Read the server log for that consumer. The log names the exception and the event position.
3. When a bug in Casebox caused the stall, update the server to a release that fixes it. The consumer tries the event again after the restart.
4. When the event cannot be applied at all, skip it. The skip is audited:

   ```bash
   deedbox skip <consumer> <event id> --wait
   ```

5. When the projection holds wrong data, rebuild it. The server keeps running and the page fills again:

   ```bash
   deedbox rebuild <projection> --wait
   ```

The projections are `workspaces`, `work_items`, `steering_facts`, `case_catalog`, `evaluation_results`, `pattern_board` and `proposal_board`. The workflows are `evaluation_workflow` and `proposal_workflow`.
