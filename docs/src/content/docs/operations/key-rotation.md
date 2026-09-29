---
title: Key rotation
description: Rotate Deedbox's master key, which wraps every organisation's keys and pseudonym secrets.
---

This runbook helps you rotate Deedbox's master key.

## When

- On your schedule, or at once when the master key may have leaked.

## Steps

1. Make a new key: `v2:$(openssl rand -base64 32)`.
2. Put the new key first in the key ring, and keep the old one: `v2:<new>,v1:<old>`. On Kubernetes, update the Secret that `keys.masterKeySecret` names and restart the server. Deedbox wraps new keys with the first key and still reads the old ones.
3. Re-wrap every tenant key and pseudonym secret with the new key. Events are not touched:

   ```bash
   deedbox keys rewrap --provider postgres --connection "$CASEBOX_DB" --from env:OLD_MASTER_KEY --to env:DEEDBOX_MASTER_KEY
   ```

4. When `rewrap` has finished, remove the old key from the ring and restart the server.

A local trial started with `casebox up` keeps its key in the database (database mode). Rotate by moving to environment or Azure Key Vault mode with the same steps.
