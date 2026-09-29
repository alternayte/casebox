---
title: Metrics
description: The metrics the server, the workers and QueueBox publish, and what each one tells an operator.
---

This page lists Casebox's own metrics. The server serves Prometheus metrics at `/metrics` on port 8081, and sends metrics and traces over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. No metric carries a person, a repository or a workspace name.

## Server

| Metric | Kind | Tags | Shows |
| --- | --- | --- | --- |
| `casebox.ingest.lag` | histogram, s | | Receipt time minus the newest event's time, per uploaded batch |
| `casebox.spool.backlog` | gauge | | Events the CLIs reported as not uploaded, over the last hour |
| `casebox.jobs.queued` | gauge | `kind` | Queued jobs |
| `casebox.jobs.oldest` | gauge, s | `kind` | How long the oldest queued job has waited |
| `casebox.jobs.lease_expiries` | counter | `kind` | Leases that expired: a worker stopped before it answered |
| `casebox.workers.seen` | gauge | | Workers seen in the last 10 minutes |
| `casebox.runs` | counter | `outcome` | Evaluation runs completed, or failed to run |
| `casebox.sandbox.start` | histogram, s | `provider` | Time to prepare and start an agent's sandbox |
| `casebox.spend.month` | gauge, USD | `org` | Evaluation spend this month |
| `casebox.budget.month` | gauge, USD | `org` | The monthly limit |
| `casebox.steering.unclassified` | gauge | `org` | Share of the last 30 days' interventions left unclassified |

The server also publishes Deedbox's metrics: `deedbox.consumer.lag.seconds` shows how far each projection and workflow is behind.

## Worker

With `OTEL_EXPORTER_OTLP_ENDPOINT` set, `casebox worker` sends a span per job, `casebox.worker.job.duration` (`kind`, `outcome`) and `casebox.worker.sandbox.start` (`provider`).

## QueueBox

QueueBox serves its own metrics at `/metrics` on port 9090, among them `queuebox_outbox_messages_total` by status: `dead` counts side effects it gave up on. See the [runbooks](/operations/).
