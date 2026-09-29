---
title: Erase a person
description: Remove a person's data from Casebox when they ask.
---

This guide erases a person. An Admin runs one command per identity the person used:

```bash
casebox erase --identity email:alice@example.com
```

The command erases every pseudonymous token that identity had in every period that still has a secret, and every alias the roster knows for the same person. It deletes their sessions, trace events and telemetry, and Deedbox deletes their keys. Their correction text becomes unreadable everywhere at once, and reports that fall below k people hide the affected groups.

The [erasure runbook](/operations/erasure-request/) covers backups and what to tell the person.
