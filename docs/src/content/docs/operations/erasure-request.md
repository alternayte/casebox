---
title: An erasure request
description: A person asks for their data to be erased.
---

## Symptom

- A person, or your data protection officer, asks Casebox to forget someone.

## Steps

1. An Admin runs, once per identity the person used:

   ```bash
   casebox erase --identity email:alice@example.com
   ```

   It erases every token that identity had in every pseudonym period that still has a secret, and every alias the roster knows for the same person (their GitHub login, their Jira account).
2. The command reports what it deleted: sessions, trace events and telemetry, by count. The organisation's history records the erasure with counts only.
3. The person's correction text, instructions and quotes become unreadable in every stream at once, because Deedbox deletes their subject keys. Reports stay consistent: themes and patterns that fall below k are hidden.
4. Backups taken before the erasure still hold the old keys until they age out. Tell the person how long your backups are kept.
