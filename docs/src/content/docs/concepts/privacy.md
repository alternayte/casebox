---
title: The privacy model and its limits
description: How Casebox measures the harness and never the person, and what it does not protect against.
sidebar:
  order: 5
---

This page explains how Casebox handles people's data, and where its protection ends.

## Pseudonyms

A person is a token, a keyed hash of their identity, such as `person:k7q2m9x4…`. The CLI never computes tokens: it marks identities, and the server replaces them in memory before anything is written. The key stays on the server, so no laptop can link a token back to a name. Tokens change every period, quarterly by default.

The roster maps a person's email, GitHub login and Jira account to one identity, in memory only. An identity the roster cannot map gets its own token and never counts toward k, because one person split in two could make a group of two look like three.

## What reports show

- Groups by workspace, repository, agent, model, harness version and task type. Never a person.
- A theme, quote, pattern or count only when at least k mapped people are behind it (default 3, at least 2).
- Quotes without authors, dated by the day.
- No route answers with a list, count or series per person.

## Erasure and retention

`casebox erase --identity` deletes a person's sessions and traces and their keys: their words become unreadable everywhere at once. When a period leaves the retention window (12 months by default), its secret is destroyed, and nobody can link that period's data to a person again. Trace events keep 180 days by default.

## Limits

- Someone with the pseudonym secret and a list of names can re-identify tokens.
- Someone with Jira access can see who was assigned a ticket at a given time.
- Backups taken before an erasure keep the old keys until they age out.
- Pseudonymous data is still personal data under the GDPR and the Swiss Federal Act on Data Protection. Casebox lowers the risk and makes erasure easy; it does not remove your legal duties.
