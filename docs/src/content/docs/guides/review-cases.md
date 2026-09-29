---
title: Review cases
description: Approve, edit or reject mined cases, so only good tasks count in an evaluation.
---

This guide reviews mined cases. A case counts in an evaluation only after a person approves it.

## On the command line

```bash
casebox review
```

For each validated case with a drafted instruction, `review` prints the source, the tests that decide it, the instruction and the interfaces the tests call. Answer:

- `a` approves it. Every fifth approved case goes to the held-out set, which only the proposer's gate uses.
- `e` opens the instruction in `$EDITOR`. Remove any hint of the solution: the instruction must ask for the change, not describe the code.
- `r` rejects it with a reason.
- `s` skips it, `q` quits.

`casebox review --approve-all` approves the whole queue and lists the cases the server refused, with the reason.

## In the web UI

The **Cases** page shows the same queue, the catalog of every case, and each case's validation runs.

![The Cases page with the demo data.](/screenshots/cases.png)

## What makes a good case

- The instruction states the task from the ticket, not from the pull request.
- The fail-to-pass tests check the behaviour the ticket asks for.
- A steering case's assertions match the correction: the file it must not touch, the command it must run before it says "done", or the pattern the diff must or must not hold. A person approves them with the case.
