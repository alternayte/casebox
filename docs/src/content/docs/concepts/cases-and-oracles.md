---
title: Cases and oracles
description: How Casebox turns your history into replayable tasks, and what decides whether an agent passed one.
sidebar:
  order: 2
---

This page explains cases: real tasks from your history, rewound to their starting commit, with an oracle that decides pass or fail without asking a model.

## Kinds

| Kind | From | Decided by |
| --- | --- | --- |
| Capability | A merged pull request with test changes | Its own tests: fail-to-pass and pass-to-pass |
| Regression | A fix of an earlier pull request | The original tests and the fix's new tests |
| Steering | A corrected session | Assertions from the correction, and the item's tests |

## The oracle

- **Fail-to-pass** tests fail at the base commit and pass with the merged change: they prove the work was done.
- **Pass-to-pass** tests in the touched packages pass both times: they catch collateral damage.
- **Steering assertions**, approved by a person: a forbidden file change, a command that must run before the agent says done, or a pattern the diff must or must not hold.
- **Judge questions** are optional yes-or-no questions to a model. They count as medium evidence and never pass a run whose tests failed.

Casebox reads results per test from `go test -json`, .NET TRX and JUnit XML, never from the exit code alone.

## Validation and sealing

A mined case counts only when the environment builds at its base commit, every fail-to-pass test fails there, and every test passes three times out of three with the merged change.

In a run, the agent works in a fresh repository with one commit and no history, no remote and no Casebox data. Its network reaches only the model API and Casebox's registry mirror, which refuses your own packages. The held-out tests never enter its sandbox: a separate verifier applies its diff to a clean copy and runs them.
