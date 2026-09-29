-- The ASP.NET Data Protection key ring, shared by every server instance: session cookies, CSRF
-- tokens and integration secrets stay valid across restarts and scale-out.
CREATE TABLE casebox.data_protection_keys (
    id         text        PRIMARY KEY,
    xml        text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One row per connected integration. The secret is Data Protection ciphertext; config holds
-- only non-secret settings.
CREATE TABLE casebox.integrations (
    org_id     text        NOT NULL,
    kind       text        NOT NULL CHECK (kind IN ('github', 'jira')),
    config     jsonb       NOT NULL,
    secret     bytea       NOT NULL,
    last_poll  jsonb,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, kind)
);

-- Last poll position per source, such as github:acme/app:pulls or jira:search.
CREATE TABLE casebox.integration_cursors (
    org_id     text        NOT NULL,
    source     text        NOT NULL,
    cursor     text        NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, source)
);

-- Work items (projection of the work_item streams).
CREATE TABLE casebox.work_items (
    org_id        text        NOT NULL,
    id            text        NOT NULL,
    provider      text        NOT NULL,
    key           text        NOT NULL,
    repo          text,
    title         text        NOT NULL,
    type          text,
    status        text,
    resolved      boolean     NOT NULL DEFAULT false,
    snapshot      jsonb,
    sessions      integer     NOT NULL DEFAULT 0,
    pull_requests integer     NOT NULL DEFAULT 0,
    merged        integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (org_id, id)
);

-- A work item's timeline: sessions, snapshots, pull requests, reviews, CI, merges, reverts, fixes.
CREATE TABLE casebox.work_item_timeline (
    org_id       text        NOT NULL,
    work_item_id text        NOT NULL,
    event_id     uuid        NOT NULL,
    at           timestamptz NOT NULL,
    kind         text        NOT NULL,
    detail       jsonb       NOT NULL,
    PRIMARY KEY (org_id, work_item_id, event_id)
);
CREATE INDEX work_item_timeline_at ON casebox.work_item_timeline (org_id, work_item_id, at);

-- The link a session has now (projection of session_linked and session_reassigned).
ALTER TABLE casebox.sessions ADD COLUMN work_item_id text;
ALTER TABLE casebox.sessions ADD COLUMN link_source text;
ALTER TABLE casebox.sessions ADD COLUMN link_confidence text;
CREATE INDEX sessions_branch ON casebox.sessions (org_id, repo, branch);

-- Low-confidence links a person may confirm: time and file overlap only.
CREATE TABLE casebox.session_link_suggestions (
    org_id       text        NOT NULL,
    session_id   text        NOT NULL,
    work_item_id text        NOT NULL,
    reason       text        NOT NULL,
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (org_id, session_id, work_item_id)
);

-- Every pull request the pollers saw, linked or not. snapshot is the tokenized poll result.
CREATE TABLE casebox.pull_requests (
    org_id       text        NOT NULL,
    repo         text        NOT NULL,
    number       integer     NOT NULL,
    state        text        NOT NULL,
    head_ref     text        NOT NULL,
    base_ref     text        NOT NULL,
    author       text        NOT NULL,
    is_agent     boolean     NOT NULL DEFAULT false,
    merged_at    timestamptz,
    merge_sha    text,
    work_item_id text,
    updated_at   timestamptz NOT NULL,
    snapshot     jsonb       NOT NULL,
    PRIMARY KEY (org_id, repo, number)
);
CREATE INDEX pull_requests_branch ON casebox.pull_requests (org_id, repo, head_ref);
CREATE INDEX pull_requests_merged ON casebox.pull_requests (org_id, repo, merged_at);

-- git-ai line attributions per commit, file and agent: which line ranges the agent wrote. The
-- ranges can be long, so they are not part of the key; the worker merges them per key.
CREATE TABLE casebox.commit_attributions (
    org_id      text        NOT NULL,
    repo        text        NOT NULL,
    sha         text        NOT NULL,
    path        text        NOT NULL,
    agent       text        NOT NULL DEFAULT '',
    model       text        NOT NULL DEFAULT '',
    agent_lines text        NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (org_id, repo, sha, path, agent, model)
);
