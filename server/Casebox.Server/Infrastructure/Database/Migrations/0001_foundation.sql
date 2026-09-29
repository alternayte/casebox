-- Accounts are the people who log in to the Casebox UI. This is account data, kept apart from
-- captured work data: no captured person ever appears here.
CREATE TABLE casebox.accounts (
    id            text        PRIMARY KEY,
    org_id        text        NOT NULL,
    issuer        text        NOT NULL,
    subject       text        NOT NULL,
    display_name  text        NOT NULL,
    role          text        NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    created_at    timestamptz NOT NULL,
    last_login_at timestamptz NOT NULL,
    UNIQUE (org_id, issuer, subject)
);

-- Worker and ingest tokens. Only the SHA-256 of a token is stored.
CREATE TABLE casebox.api_tokens (
    id           text        PRIMARY KEY,
    org_id       text        NOT NULL,
    kind         text        NOT NULL CHECK (kind IN ('worker', 'ingest')),
    name         text        NOT NULL,
    token_hash   bytea       NOT NULL UNIQUE,
    created_by   text        NOT NULL,
    created_at   timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX api_tokens_org ON casebox.api_tokens (org_id, created_at);

-- Queued work with leases. Workers lease over HTTP; the server picks with FOR UPDATE SKIP LOCKED.
CREATE TABLE casebox.jobs (
    id               text        PRIMARY KEY,
    org_id           text        NOT NULL,
    kind             text        NOT NULL,
    idempotency_key  text        NOT NULL,
    payload          jsonb       NOT NULL,
    status           text        NOT NULL CHECK (status IN ('queued', 'leased', 'succeeded', 'failed')),
    attempts         integer     NOT NULL DEFAULT 0,
    max_attempts     integer     NOT NULL,
    available_at     timestamptz NOT NULL,
    lease_owner      text,
    lease_expires_at timestamptz,
    result           jsonb,
    last_error       text,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    UNIQUE (org_id, idempotency_key)
);
CREATE INDEX jobs_ready ON casebox.jobs (org_id, kind, available_at) WHERE status = 'queued';
CREATE INDEX jobs_leased ON casebox.jobs (lease_expires_at) WHERE status = 'leased';

-- The workers each organisation has seen, for health and the Settings page.
CREATE TABLE casebox.workers (
    org_id       text        NOT NULL,
    worker_id    text        NOT NULL,
    version      text        NOT NULL,
    last_seen_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, worker_id)
);

-- Content-addressed blobs, zstd-compressed. The S3-compatible store keeps only this index row.
CREATE TABLE casebox.blobs (
    org_id       text        NOT NULL,
    hash         text        NOT NULL,
    size         bigint      NOT NULL,
    content_type text        NOT NULL,
    data         bytea,
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (org_id, hash)
);

-- Workspace projection: the read side of the workspace streams.
CREATE TABLE casebox.workspaces (
    org_id        text        NOT NULL,
    id            text        NOT NULL,
    name          text        NOT NULL,
    repos         jsonb       NOT NULL,
    recipe_status text        NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (org_id, id)
);
