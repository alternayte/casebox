---
title: Outbox dead letters
description: A side effect failed 8 times and QueueBox stopped delivering it.
---

## Symptom

- A harness CI comment or a proposal's pull request never appears.

## What shows it

- `queuebox_outbox_messages_total{status="dead"}` on QueueBox's `/metrics` increases.

## Steps

1. List the dead messages and their last error:

   ```sql
   SELECT id, topic, attempt, updated_at, last_error FROM outbox WHERE state = 'dead' ORDER BY updated_at DESC;
   ```

2. Fix the cause. The usual causes are an expired GitHub credential (see [An expired integration token](../expired-integration-token/)) or a missing permission: comments need pull requests write, proposals also need contents write.
3. Replay the dead messages by ID through QueueBox's admin route on its management port. Casebox's effect handlers are idempotent: a comment is updated in place and a pull request opens once, so a replay never duplicates them.

   ```bash
   curl -X POST -H "Authorization: Bearer $CASEBOX_QUEUEBOX_ADMIN_TOKEN" -H 'Content-Type: application/json' \
     -d '{"ids": ["<id>"]}' http://<queuebox>:9090/admin/replay
   ```

See QueueBox's guide [Replay dead letters](https://queuebox-docs.pages.dev/how-to/replay-dead-letters/) for the filters.
