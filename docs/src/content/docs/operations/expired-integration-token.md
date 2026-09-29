---
title: An expired integration token
description: GitHub or Jira stop answering, so work items, pull requests, comments and proposals stop.
---

This runbook helps you replace an expired GitHub or Jira credential.

## Symptom

- New pull requests and Jira issues do not appear on the Work page.
- Harness CI comments or proposal pull requests do not appear.

## What shows it

- The Integrations settings show the last poll's error.
- The server log shows `CBX071` (GitHub) or `CBX072` (Jira).
- Outbox messages for `effect.ci_comment` or `effect.proposal_pr` go dead (see [Outbox dead letters](/operations/outbox-dead-letters/)).

## Steps

1. Create a new credential. GitHub: a fine-grained token with metadata, contents, issues, checks and pull requests, or check the App's installation. Jira Data Center: a new personal access token.
2. Connect it again with `casebox init` (an Admin). The server checks it against GitHub or Jira before it stores it.
3. The pollers catch up from their cursors on the next run. Replay the dead comments and pull requests after the fix.
