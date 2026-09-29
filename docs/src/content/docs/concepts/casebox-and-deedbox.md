---
title: Casebox and Deedbox
description: How Casebox keeps its history as events with Deedbox, and takes its inputs through QueueBox.
sidebar:
  order: 6
---

This page explains how Casebox stores its decisions. Casebox is built on [Deedbox](https://deedbox-docs.pages.dev/) and [QueueBox](https://queuebox-docs.pages.dev/), on one Postgres database.

- **Every business decision is an event.** A correction's label, a pattern's dismissal and a proposal's approval, rejection and apply are events in their own streams. A decider checks each rule before an event is appended, such as "only an approved proposal is applied" or "a rejection needs a reason". The [event catalogue](/reference/events/) lists them.
- **Pages are projections.** The steering facts and the pattern and proposal boards are rebuilt from the events at any time, with the same result.
- **Personal data is encrypted per person.** A correction's text is encrypted with its person's key. Erasing the person deletes the key, and the text becomes unreadable in every stream at once.
- **Every input enters through the inbox.** GitHub webhooks and poll results are deduplicated by QueueBox before Casebox appends anything.

High-volume data stays in plain tables: session trace events and pull request snapshots. Casebox writes nothing to a git host: a proposal lands through `casebox apply` on a person's machine.
