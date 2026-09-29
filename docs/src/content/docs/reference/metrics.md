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
| `casebox.steering.unclassified` | gauge | `org` | Share of the last 30 days' interventions left unclassified |

The server also publishes Deedbox's metrics: `deedbox.consumer.lag.seconds` shows how far each projection and workflow is behind.

## Worker

With `OTEL_EXPORTER_OTLP_ENDPOINT` set, `casebox worker` sends a span per job and `casebox.worker.job.duration` (`kind`, `outcome`).

## QueueBox

QueueBox serves its own metrics at `/metrics` on port 9090, among them the inbox's received and handled messages. See the [runbooks](/operations/).
