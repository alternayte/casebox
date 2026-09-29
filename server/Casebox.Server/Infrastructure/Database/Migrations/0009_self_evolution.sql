-- Self-evolution (docs/specs/self-evolution.md): the pattern board, the proposal board, and the
-- top-level path of each correction, which groups corrections into patterns.
CREATE TABLE casebox.patterns (
    org_id      text        NOT NULL,
    id          text        NOT NULL,
    workspace   text        NOT NULL,
    went_wrong  text        NOT NULL,
    label       text,
    prevention  text        NOT NULL,
    path        text        NOT NULL,
    title       text        NOT NULL,
    summary     text        NOT NULL,
    refs        jsonb       NOT NULL,
    advisory    boolean     NOT NULL,
    status      text        NOT NULL,
    reason      text,
    detected_at timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (org_id, id)
);
CREATE INDEX patterns_workspace ON casebox.patterns (org_id, workspace);

CREATE TABLE casebox.correction_paths (
    org_id          text NOT NULL,
    stream_id       text NOT NULL,
    intervention_id text NOT NULL,
    path            text NOT NULL,
    PRIMARY KEY (org_id, stream_id, intervention_id)
);

CREATE TABLE casebox.proposals (
    org_id          text        NOT NULL,
    id              text        NOT NULL,
    workspace       text        NOT NULL,
    pattern         text,
    kind            text        NOT NULL,
    status          text        NOT NULL,
    repo            text        NOT NULL,
    base_commit     text        NOT NULL,
    gate_index      integer,
    gate_evaluation text,
    checks          jsonb,
    reason          text,
    pr_number       integer,
    pr_url          text,
    branch          text,
    merged_at       timestamptz,
    outcome         jsonb,
    ci_run          text,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (org_id, id)
);
CREATE INDEX proposals_pattern ON casebox.proposals (org_id, pattern);

CREATE TABLE casebox.proposal_candidates (
    org_id       text    NOT NULL,
    proposal_id  text    NOT NULL,
    idx          integer NOT NULL,
    edits        jsonb   NOT NULL,
    files        jsonb   NOT NULL,
    overrides    text    NOT NULL,
    content_hash text    NOT NULL,
    rationale    text    NOT NULL,
    merged_from  jsonb,
    score        jsonb,
    PRIMARY KEY (org_id, proposal_id, idx)
);
CREATE INDEX proposal_candidates_hash ON casebox.proposal_candidates (org_id, content_hash);
