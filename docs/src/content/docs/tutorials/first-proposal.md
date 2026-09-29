---
title: Your first proposal
description: Approve a change that prevents a recurring correction, apply it privately, and see whether the corrections fall.
sidebar:
  order: 2
---

In this tutorial you read a proposal that Casebox drafted from your corrections, approve it, and apply it to your repository without changing anything your team sees.

You need the setup of [your first steering report](/tutorials/first-steering-report/), a worker with an analysis model, and at least 3 similar corrections. Casebox needs the worker to read your repository's current instruction files: for a private GitHub repository, give the worker a `GITHUB_TOKEN` with read access.

## 1. Find the proposal

After an import, the worker classifies your interventions, groups similar corrections into patterns, and drafts a proposal for each new pattern. In the repository:

```bash
casebox proposals
```

Each line shows the proposal's ID, its status, its kind and its title, and the pattern it comes from. The **Proposals** page in the web UI shows the same list.

## 2. Read it

```bash
casebox proposals show <id>
```

It shows the corrections behind the proposal in your own words, why the change prevents them, and the change as a diff. A code note shows the prompt that your agent gets instead.

## 3. Approve or reject it

```bash
casebox proposals approve <id>
casebox proposals reject <id> --reason "we do not use that service"
```

A rejection keeps the change from coming back for 90 days, and the next draft reads your reason. At most three proposals wait for you at a time.

## 4. Apply it

```bash
casebox apply <id>
```

By default the change stays private: it is written as its own file and listed in `.git/info/exclude`. `git status` stays clean, your agent reads the file from its next session, and nobody else sees it. When you want your team to have it, run `casebox apply <id> --commit`, then commit the change and open a pull request as usual.

## 5. See whether it helped

Keep working as usual. 30 days after you apply a proposal, `casebox proposals --all` and the proposal's page show its pattern's corrections per 100 sessions before and after. The comparison is observational.

## Next

- [Patterns and proposals](/concepts/proposals/): how drafts are made, and what Casebox never changes.
