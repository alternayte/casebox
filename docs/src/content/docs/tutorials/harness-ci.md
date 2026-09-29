---
title: Harness CI on a repository
description: Add the GitHub Action so every pull request that changes the harness gets a measured comment before it merges.
sidebar:
  order: 3
---

In this tutorial you add Casebox's GitHub Action to a repository. A pull request that changes `AGENTS.md`, rules or skills then runs a smoke suite, and the result arrives as a comment. A nightly run keeps the default branch's score current.

You need approved cases (see [your first comparison](/tutorials/first-comparison/)), a Casebox server that GitHub's runners can reach, and a GitHub connection with pull requests write.

## 1. Issue a CI token

An Admin runs:

```bash
casebox token create --kind ci --name "harness CI for acme/payments"
```

A ci token starts CI runs and runs their jobs, and reads nothing else.

## 2. Add the secrets and the workflow

In the repository's settings on GitHub, add:

- the variable `CASEBOX_SERVER`: your server's URL;
- the secret `CASEBOX_TOKEN`: the ci token;
- the secret `ANTHROPIC_API_KEY` (or your agent's key), when the Action runs the jobs itself.

Copy `examples/harness-ci.yml` from the Casebox repository to `.github/workflows/harness-ci.yml`. It runs on pull requests that change harness files, every night for the baseline, and every week for the proposer. With `worker: inline` the runner does the work on its own Docker; without it, your organisation's workers do.

## 3. Score the baseline once

Run the workflow by hand (**Actions → Harness CI → Run workflow**). It scores the default branch's harness on your dev cases. The nightly run then scores only what changed: a new harness, a new model, new cases, or a score older than 7 days.

## 4. Open a pull request

Change a line of `AGENTS.md` and open a pull request. The Action runs the smoke suite, 10 cases once, with the pull request's harness. The comment gives:

- the regressions: cases the baseline passed every time that now fail;
- the verdict, the interval, and the difference this size can detect;
- the cost, and the command for a full comparison.

The comment looks like this one, for a change that broke one case:

<div id="harness-ci-comment" class="gh-comment">

### Casebox harness CI: workspace `payments` at `8d2f41c09a7e`

**1 regression.** The baseline passed this case in every run, and the candidate failed in every run.

- Verdict: **inconclusive**. The 95% interval neither lies on one side of 0 nor within ±5 points.
- Δ pass rate (candidate − baseline): -6.7 points, 95% interval -39.0 points to +21.0 points, over 10 cases and 10 candidate runs.
- At this size the comparison detects a difference of about 58 points or more (power 0.8). A smoke run does not claim "better".
- Cost: 5.87 USD of an estimated 6.20 USD; 0 runs failed to run.
- Baseline: the default branch's harness `c41e9b0d7a2f`, scored 2026-09-28 by the nightly run, not run again here.

| Case | Baseline passed | Candidate passed | |
| --- | --- | --- | --- |
| `3f9a1c01e7b2d4` | 3/3 | 1/1 |  |
| `3f9a1c02e7b2d4` | 2/3 | 1/1 |  |
| `3f9a1c03e7b2d4` | 3/3 | 1/1 |  |
| `3f9a1c04e7b2d4` | 3/3 | 0/1 | regression |
| `3f9a1c05e7b2d4` | 3/3 | 1/1 |  |
| `3f9a1c06e7b2d4` | 2/3 | 1/1 |  |
| `3f9a1c07e7b2d4` | 3/3 | 1/1 |  |
| `3f9a1c08e7b2d4` | 2/3 | 1/1 |  |
| `3f9a1c09e7b2d4` | 3/3 | 1/1 |  |
| `3f9a1c10e7b2d4` | 2/3 | 1/1 |  |

A smoke run compares the candidate with a cached baseline. For a verdict with its full interval, run `casebox compare --candidate harness=8d2f41c09a7e3b5f6e1d2c3b4a596877` in a checkout of this pull request.
Details: https://casebox.example.com/evaluations/01JDOCSEVAL

</div>

A smoke run never claims "better": ten cases cannot show a small improvement. The job passes unless you set `fail-on: regression`.

## Next

- [Self-evolution and the gate](/concepts/self-evolution/): let Casebox propose harness edits from recurring corrections.
