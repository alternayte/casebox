---
title: Casebox and Deedbox
description: How Casebox keeps its history as events with Deedbox, and its side effects and inputs with QueueBox.
sidebar:
  order: 6
---

This page explains how Casebox stores its decisions. Casebox is built on [Deedbox](https://deedbox-docs.pages.dev/) and [QueueBox](https://queuebox-docs.pages.dev/), on one Postgres database.

- **Every business decision is an event.** A case's approval, an evaluation's verdict, a pattern's dismissal and a proposal's gate are events in their own streams. A decider checks each rule before an event is appended, such as "no run after a verdict" or "a pull request opens only after the gate passed". The [event catalogue](/reference/events/) lists them.
- **Pages are projections.** The steering facts, the case catalog, the evaluation results and the pattern and proposal boards are rebuilt from the events at any time, with the same result.
- **Personal data is encrypted per person.** A correction's text is encrypted with its person's key. Erasing the person deletes the key, and the text becomes unreadable in every stream at once.
- **Every side effect leaves through the outbox.** A harness CI comment and a proposal's pull request are rows in QueueBox's outbox, written in the same transaction as their event, so an effect exists exactly when its event does. The handlers are idempotent, so a redelivery never duplicates a comment or a pull request.
- **Every input enters through the inbox.** GitHub webhooks and poll results are deduplicated by QueueBox before Casebox appends anything.

High-volume data stays in plain tables: session trace events, run results and blobs.
