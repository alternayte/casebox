---
title: Connect GitHub with a token or an App
description: Choose between a fine-grained token (polling) and a GitHub App (webhooks), and connect it.
---

This guide connects GitHub, which gives Casebox pull requests, reviews, checks, reverts and, if you want them, issues. Choose a token for a trial and an App for an organisation.

| | Fine-grained token | GitHub App |
| --- | --- | --- |
| Updates | A poll every 5 minutes | Webhooks, and the poll as a safety net |
| Who creates it | Any member with access to the repositories | An organisation owner |
| Comments and proposals | As the token's owner | As the App |

Both need: metadata and contents (read), issues (read), checks (read), and pull requests (read and write). Proposals push a branch, so they also need contents write.

## A token

```bash
export CASEBOX_GITHUB_TOKEN=<fine-grained token>
casebox init
```

The server checks the token before it stores it (`CBX071` when GitHub refuses it). Add `--github-issues` to track GitHub Issues as work items.

## An App

1. Create a GitHub App with the permissions above and these webhook events: pull request, pull request review, pull request review comment, check run, workflow run, push and issues. Set the webhook URL to `https://<your server>/webhooks/github` and choose a webhook secret.
2. Install the App on the repositories, and note the App ID and the installation ID.
3. As an Admin, connect it:

   ```bash
   casebox api PUT /api/v1/integrations/github --data '{"mode":"app","appId":123,"installationId":456,"privateKey":"<PEM>","webhookSecret":"<secret>"}'
   ```

The server checks each delivery's signature before QueueBox stores it, and QueueBox stores a redelivered webhook once.
