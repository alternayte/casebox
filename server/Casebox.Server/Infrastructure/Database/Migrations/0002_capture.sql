-- CLI tokens belong to one account and act with its current role.
ALTER TABLE casebox.api_tokens DROP CONSTRAINT api_tokens_kind_check;
ALTER TABLE casebox.api_tokens ADD CONSTRAINT api_tokens_kind_check CHECK (kind IN ('worker', 'ingest', 'cli'));
ALTER TABLE casebox.api_tokens ADD COLUMN account_id text;
ALTER TABLE casebox.api_tokens ADD CONSTRAINT api_tokens_cli_account CHECK ((kind = 'cli') = (account_id IS NOT NULL));

-- Device login (RFC 8628). Only the SHA-256 of the device code is stored.
CREATE TABLE casebox.device_codes (
    device_code_hash bytea       PRIMARY KEY,
    user_code        text        NOT NULL UNIQUE,
    org_id           text        NOT NULL,
    status           text        NOT NULL CHECK (status IN ('pending', 'approved', 'consumed')),
    account_id       text,
    expires_at       timestamptz NOT NULL,
    created_at       timestamptz NOT NULL
);

-- Session metadata: one row per session. person is a subject ID, never an identity.
CREATE TABLE casebox.sessions (
    org_id        text        NOT NULL,
    id            text        NOT NULL,
    agent         text        NOT NULL,
    agent_version text,
    model         text,
    repo          text,
    branch        text,
    head_start    text,
    head_end      text,
    person        text        NOT NULL,
    work_item     text,
    source        text        NOT NULL,
    started_at    timestamptz NOT NULL,
    ended_at      timestamptz,
    event_count   integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (org_id, id)
);
CREATE INDEX sessions_repo ON casebox.sessions (org_id, repo, started_at);
CREATE INDEX sessions_person ON casebox.sessions (org_id, person);

-- Canonical trace events, partitioned by month for retention. The ingest creates each month's
-- partition on first use.
CREATE TABLE casebox.session_events (
    org_id     text        NOT NULL,
    session_id text        NOT NULL,
    seq        bigint      NOT NULL,
    at         timestamptz NOT NULL,
    kind       text        NOT NULL,
    text       text,
    tool       jsonb,
    usage      jsonb,
    attrs      jsonb       NOT NULL DEFAULT '{}',
    PRIMARY KEY (org_id, session_id, seq, at)
) PARTITION BY RANGE (at);

-- Native OpenTelemetry metric data points. Identity attributes are dropped before the write.
CREATE TABLE casebox.session_metrics (
    org_id     text             NOT NULL,
    session_id text,
    agent      text             NOT NULL,
    name       text             NOT NULL,
    value      double precision NOT NULL,
    at         timestamptz      NOT NULL,
    attrs      jsonb            NOT NULL DEFAULT '{}'
);
CREATE INDEX session_metrics_session ON casebox.session_metrics (org_id, session_id, name);
CREATE INDEX session_metrics_at ON casebox.session_metrics (at);
