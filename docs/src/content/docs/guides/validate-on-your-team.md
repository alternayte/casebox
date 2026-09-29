---
title: Validate Casebox on your team
description: Run the steering report and a first measured comparison on your own repository, and check that the labels and the cost estimate hold.
---

This guide runs Casebox on one of your team's repositories and answers two questions:

1. Is the steering report right? Are the themes the ones your team recognises, and does the classifier agree with a person?
2. Does a real comparison hold up? Is the verdict readable, and is the cost estimate close to the actual cost?

You need Docker, git, Go 1.26, a GitHub token that reads the repository, a model key for the agent you use (Anthropic, OpenAI or Cursor), and an analysis model key (any Anthropic or OpenAI-compatible API).

## 1. Install

Until release binaries and images exist, build both from the source:

```bash
git clone https://github.com/alternayte/casebox && cd casebox
(cd cli && go build -o ~/bin/casebox ./cmd/casebox)
docker build -f server/Dockerfile -t casebox-server:local .
casebox up --image casebox-server:local
```

`casebox up` prints the admin password and the URL, `http://localhost:8080`.

To see every page first, with a synthetic team, run `casebox up --demo --image casebox-server:local` on a fresh machine. The demo loads only while the organisation has no session of its own.

## 2. Enrol the repository and import

In the repository:

```bash
casebox init            # log in, choose the prompt mode (redacted is suggested), connect GitHub and Jira
casebox import --days 30
```

Each teammate who wants to be counted runs `casebox join` in the repository. A theme shows only when 3 people are behind it, so a report of one person's sessions stays hidden.

## 3. Classify with a real model

Start a worker with your analysis model and the GitHub token:

```bash
export CASEBOX_WORKER_TOKEN=$(casebox token create --kind worker --name "my laptop")
export GITHUB_TOKEN=<token>
export CASEBOX_ANALYSIS_PROVIDER=anthropic CASEBOX_ANALYSIS_MODEL=<model id> ANTHROPIC_API_KEY=<key>
casebox worker
```

The Overview page fills as interventions are classified.

## 4. Check the steering report (gate 2)

Open Overview and read the top correction themes with the team.

- For each theme, do its quotes show the same kind of correction? Relabel what is wrong on the theme's page. Relabels are examples for later runs.
- Run `casebox steering check` for the classifier's agreement on Casebox's labelled set.
- Once a person has relabelled about 50 events, run `casebox steering check --team` for the agreement with your team's own labels.

Write down: the themes you agree with, the ones you do not, and the agreement numbers.

## 5. Make cases

```bash
casebox env check      # build the environment and run the tests at HEAD
casebox env confirm
casebox mine
casebox review         # approve or reject each validated case
```

Approve at least 10 cases. Every fifth approved case goes to the held-out set.

## 6. Run your harness against no harness (gate 3)

Add the agent and the prices to `.casebox/casebox.yml`:

```yaml
evaluation:
  baseline: { agent: claude-code, agent_version: <version>, model: <model id> }
prices:
  <model id>: { input: <USD per million>, output: <USD per million>, cache_read: <USD per million> }
```

Start a worker that runs agents (it needs Docker and the model key), then:

```bash
casebox compare --candidate harness=none
```

`compare` prints the estimate first: runs, tokens, cost, time, and the smallest difference this size can detect. It asks before it starts.

Write down:

- the estimated cost and the actual spend, which the Evaluations page shows side by side;
- the verdict, its interval and its sample size;
- whether the verdict matches what the team expected.

A difference of more than 30% between the estimate and the actual cost means the default token numbers do not fit your cases. The next estimate uses the runs of this one.

## What to send back

The notes of steps 4 and 6 are the evidence for gates 2 and 3.
