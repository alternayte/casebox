---
title: How verdicts work
description: What better, worse, equivalent and inconclusive mean, in plain words, and why a small evaluation often says inconclusive.
sidebar:
  order: 3
---

This page explains how Casebox decides a verdict, and how to read one.

## The comparison

An evaluation changes one thing: the model, the agent, the effort, or the harness. Both sides run the same cases, in random interleaved order, so a slow afternoon at the model provider hits both alike. Each case's pass rate comes from its repeats. The result is the mean difference in pass rate across cases, **Δ**, with an interval from a bootstrap over the cases.

## The rules

| Verdict | Rule |
| --- | --- |
| Better | The interval lies entirely above 0. |
| Worse | The interval lies entirely below 0. |
| Equivalent | The interval lies within ±δ (5 points by default). |
| Inconclusive | Anything else, fewer than 10 cases, or the budget ran out. |

"Equivalent and cheaper" is a result in its own right: the same quality at a lower cost.

An inconclusive verdict whose interval straddles 0 within ±2δ reads **no difference detected**: any real difference is small, but the run cannot show that it is below δ.

## Size and power

Every estimate shows the smallest difference the evaluation can detect with power 0.8. At 3 repeats, 120 cases detect a 10-point difference; 30 cases detect about 20 points. A smaller real difference then likely comes out inconclusive. That is not "no effect"; it is "not measurable at this size".

## Stopping early

The runner checks after each full round of repeats and stops at the first clear verdict. To keep false verdicts at 5% or below over all the checks, the checks before the last use 99.9% intervals and the last check uses what remains of 5%. Casebox's simulations of the whole procedure measure 5.0% to 5.6% false "better or worse" with no real difference.
