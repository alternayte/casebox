---
title: Self-evolution and the gate
description: How recurring corrections become small harness edits, and how the held-out gate decides which ones reach a pull request.
sidebar:
  order: 4
---

This page explains how Casebox proposes harness changes and proves them before a person sees them.

## Patterns

Corrections that share what went wrong, what prevents it next time and the area of the code form a group. The analysis model splits each group by cause. A part with at least 3 corrections from k people within 60 days becomes a pattern. A person can acknowledge or dismiss it with a reason. Patterns that need a clearer ticket, a stronger model or tool access get an advisory note: Casebox does not change those by pull request.

![A pattern page with the demo data: the corrections, their quotes and the proposals.](/screenshots/pattern.png)

## Proposals

For a pattern, the proposer drafts 3 to 5 small edits: a bullet under a heading of `AGENTS.md`, a sharper rule, a removed section, or a short skill. It never rewrites a whole file. Each candidate runs on a small batch of dev cases against the cached baseline. Two winners that change different files are merged and scored too. The search stops at its budget, 40 runs per pattern by default.

The harness diet runs once a quarter: it tries removing each large section and skill, and proposes a removal that keeps quality at a lower cost.

## The gate

The best candidate runs against the baseline on the held-out set. It passes only when:

- its verdict is better or equivalent;
- the pattern's own held-out cases improve, or it is equivalent and cheaper;
- the working habits do not get worse: running the tests before saying done, and not editing a test after a failure.

The proposer never sees a held-out case or its result, only the gate's outcome. The held-out set answers 10 gate queries, then rotates in fresh cases.

![A proposal page with the demo data: the candidates, the three gate checks and the pull request.](/screenshots/proposal.png)

## The pull request

A passing gate opens one pull request with the change, the evidence (counts, people, quotes without authors, cases), the gate report, and how to undo it: revert the pull request. A person merges it. For 30 days after the merge, Casebox compares the pattern's correction rate before and after, as an observational number. A rejected change is not proposed again for 90 days, and the proposer reads the reason.
