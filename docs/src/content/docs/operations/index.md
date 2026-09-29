---
title: Operations
description: Runbooks for operating a Casebox server, its workers and QueueBox.
---

This page lists the runbooks for a Casebox server, its workers and QueueBox. Each runbook starts with the symptom, then the metric or health check that shows it, then the steps. Metrics come from `/metrics` on port 8081 of the server and port 9090 of QueueBox. Health is `/healthz/ready` and `/healthz/live` on port 8081.

- [A stalled projection](/operations/stalled-projection/)
- [A stuck job](/operations/stuck-job/)
- [A worker offline](/operations/worker-offline/)
- [A sandbox provider down](/operations/sandbox-provider-down/)
- [An expired integration token](/operations/expired-integration-token/)
- [Outbox dead letters](/operations/outbox-dead-letters/)
- [An erasure request](/operations/erasure-request/)
- [Key rotation](/operations/key-rotation/)
