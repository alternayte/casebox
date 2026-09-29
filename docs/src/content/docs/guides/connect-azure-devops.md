---
title: Connect Azure DevOps Server
description: Read pull requests, review threads, checks and reverts from Azure DevOps Server 2022 or later with a read-only personal access token.
---

This guide connects a repository on Azure DevOps Server 2022 or later. Casebox then reads its pull requests, review threads, votes, statuses, build policies and reverts, as it does on GitHub. Casebox writes nothing to Azure DevOps. Work items still come from Jira or GitHub Issues.

Azure DevOps Services (dev.azure.com) and Azure DevOps Server 2020 or earlier are not supported. The server reads each collection with a personal access token. It does not use NTLM or Kerberos.

## 1. Create a token

In the collection, open **User settings**, then **Personal access tokens**, and create a token with these scopes:

- **Code (Read)**: pull requests, commits, statuses and policies.
- **Identity (Read)**: the mail and name of each author and reviewer. The server uses them in memory to join a reviewer to their sessions, and stores neither. Without this scope, the people stay unmapped and count toward no k.

## 2. Enrol the repository

In a clone of the repository, run:

```bash
export CASEBOX_AZURE_DEVOPS_TOKEN=<token>
casebox init
```

`init` finds the collection URL in the remote, such as `https://ado.example.com/tfs/DefaultCollection` for `https://ado.example.com/tfs/DefaultCollection/Payments/_git/payments-api`. Press Enter to keep it, or type the correct URL. Use `--ado-collection` to give it without the prompt. The server checks the token against the collection before it stores it (`CBX073` when the collection refuses it). Without a token, `init` enrols the repository and the server reads no pull requests.

Casebox names the repository by its remote without `/_git`: `ado.example.com/tfs/defaultcollection/payments/payments-api`.

## 3. Check it

```bash
casebox doctor
```

Under **This repository**, `doctor` shows whether the collection answers the stored token, and whether the token can read identities.

## 4. Give the worker a token

The worker clones the repository to read commits and the current harness files. Give it a token with Code (Read):

```bash
export AZURE_DEVOPS_TOKEN=<token>
casebox worker
```

## What Casebox reads

The server polls every 5 minutes. Azure DevOps has no "changed since" query in API 7.0, so every poll reads each active pull request, and the pull requests completed or abandoned since the last poll.

- A completed pull request is merged at its close time. Its merge commit is the `Merged PR <n>:` commit on the default branch.
- A revert is a pull request whose title starts with `Revert`, or a commit on the default branch that says `Revert` and names `Merged PR <n>`.
- A review comment on a line is a review comment. A vote is a review: approve (10 or 5), or wait and reject (-5 or -10).
- Statuses and build policy runs are checks.
- Build services, groups and identities without mail are bots. They never count toward k.

Identity IDs, Windows accounts (`DOMAIN\user`), mail and display names stay in memory. The pull request and its comments keep pseudonymous tokens only.
