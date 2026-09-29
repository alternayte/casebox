---
title: Rotate keys
description: Rotate the master key that protects every organisation's keys and pseudonym secrets.
---

This guide points to the steps for rotating Deedbox's master key, which wraps each organisation's keys and pseudonym secrets. Follow the [key rotation runbook](/operations/key-rotation/): add the new key first in the ring, re-wrap with `deedbox keys rewrap`, then remove the old key.

Pseudonym secrets rotate on their own every period (quarterly by default): a person's token changes, so distinct-people counts hold within one period. When a period leaves the retention window, its secret is destroyed and its data can no longer be linked to anyone.
