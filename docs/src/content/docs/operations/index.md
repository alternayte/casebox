---
title: Operations
description: Runbooks for operating a Casebox server, its workers and QueueBox.
---

Each runbook starts with the symptom, then the metric or health check that shows it, then the steps. Metrics come from `/metrics` on port 8081 of the server and port 9090 of QueueBox. Health is `/healthz/ready` and `/healthz/live` on port 8081.

- [A stalled projection](./stalled-projection/)
- [A stuck job](./stuck-job/)
- [A worker offline](./worker-offline/)
- [A sandbox provider down](./sandbox-provider-down/)
- [An expired integration token](./expired-integration-token/)
- [Outbox dead letters](./outbox-dead-letters/)
- [An erasure request](./erasure-request/)
- [Key rotation](./key-rotation/)
