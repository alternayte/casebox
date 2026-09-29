---
title: Steering and how it is classified
description: What counts as a correction, how Casebox finds and labels interventions, and why every number is observational.
sidebar:
  order: 1
---

This page explains steering: how often, where and why people correct their agents' work. It is Casebox's headline number.

## Interventions

Casebox finds interventions from the structure of sessions and pull requests first, then classifies only those.

| Signal | Phase |
| --- | --- |
| A follow-up message after an agent turn, an interruption, a denied tool call, a rewind, a human edit between turns | In session |
| A session abandoned, or restarted with another agent or model | In session |
| A human commit that rewrites lines the agent wrote, a review comment followed by a change to those lines, a human fix of failed CI | Before merge |
| A revert, or a fix within 30 days that changes lines of the agent's pull request | After merge |

After-merge corrections cost the most, so the report shows them apart.

## Labels

A worker labels each intervention with the team's analysis model, in three steps:

1. **Intent**: a correction fixes what the agent did; a direction adds or changes scope; a clarification answers the agent; routine is an expected approval. Only corrections count as steering.
2. **What went wrong**: a missed requirement, a broken convention, a wrong approach, done without verifying, the wrong area, over-engineering, missing domain knowledge, an environment problem, or other.
3. **What prevents it next time**: an instruction, a skill, tool access, a verification step, a clearer ticket, a stronger model, or nothing in the harness.

The model sees only the window around the event, with every person shown as `[person]`. It answers with a confidence; below 0.6 the event stays unclassified. A person can relabel any event, and relabels become examples for later runs. `casebox steering check` measures the model's agreement on Casebox's labelled set, and `--team` against your own relabels.

## Numbers

Every steering number is observational: it shows what happened, not why. A difference between two groups is not a cause. Every number shows its sample size, and a group shows only with at least k people behind it. For cause and effect, run an [evaluation](/concepts/verdicts/).
