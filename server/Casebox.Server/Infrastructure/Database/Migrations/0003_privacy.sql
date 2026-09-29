-- Whether a session's person token came from the roster (one person, one token) or from an
-- identity the roster could not map. Unmapped tokens never count toward k.
ALTER TABLE casebox.sessions ADD COLUMN person_mapped boolean NOT NULL DEFAULT false;

-- The pseudonym period the person token belongs to, for erasure and retention.
ALTER TABLE casebox.sessions ADD COLUMN period text;
UPDATE casebox.sessions SET period = to_char(started_at AT TIME ZONE 'UTC', 'YYYY') || '-Q' || to_char(started_at AT TIME ZONE 'UTC', 'Q') WHERE period IS NULL;
ALTER TABLE casebox.sessions ALTER COLUMN period SET NOT NULL;
CREATE INDEX sessions_period ON casebox.sessions (org_id, period);

-- Set when the session's period left the retention window and its secret was destroyed.
ALTER TABLE casebox.sessions ADD COLUMN period_retired_at timestamptz;
