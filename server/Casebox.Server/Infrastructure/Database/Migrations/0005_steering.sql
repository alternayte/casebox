-- What the steering detector and the report need of a session: its harness version, how many
-- commits its person made during it (null when unknown), its task type when the classifier gave
-- one, and how far the detector has read it.
ALTER TABLE casebox.sessions ADD COLUMN harness_hash text;
ALTER TABLE casebox.sessions ADD COLUMN harness_version text;
ALTER TABLE casebox.sessions ADD COLUMN commits integer;
ALTER TABLE casebox.sessions ADD COLUMN task_type text;
ALTER TABLE casebox.sessions ADD COLUMN steering_scanned_at timestamptz;
ALTER TABLE casebox.sessions ADD COLUMN steering_final_at timestamptz;
CREATE INDEX sessions_steering ON casebox.sessions (org_id, updated_at) WHERE steering_scanned_at IS NULL OR steering_scanned_at < updated_at;

-- Harness versions: the hash of the harness files plus the agent and the model. Immutable.
CREATE TABLE casebox.harness_versions (
    org_id     text        NOT NULL,
    hash       text        NOT NULL,
    files_hash text        NOT NULL,
    files      jsonb       NOT NULL,
    agent      text        NOT NULL,
    model      text        NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, hash)
);

-- changed_at moves whenever Casebox learns something new about a pull request (a poll, or an
-- attribution that makes it an agent pull request); the detector reads it again then.
ALTER TABLE casebox.pull_requests ADD COLUMN changed_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE casebox.pull_requests ADD COLUMN steering_scanned_at timestamptz;

-- Every check result seen on a pull request's commits, across polls: a poll sees only the checks
-- of the commits it reads, and a CI fix needs the failure that came before it.
CREATE TABLE casebox.pr_checks (
    org_id       text        NOT NULL,
    repo         text        NOT NULL,
    number       integer     NOT NULL,
    sha          text        NOT NULL,
    name         text        NOT NULL,
    conclusion   text        NOT NULL,
    completed_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, repo, number, sha, name, conclusion)
);

-- The job kinds each worker leases, so the report can say whether a worker with an analysis
-- model is running.
ALTER TABLE casebox.workers ADD COLUMN kinds text[] NOT NULL DEFAULT '{}';

-- One row per intervention (projection of the steering streams): its current labels and where
-- they came from. text holds tokens, never an identity; the API masks them.
CREATE TABLE casebox.steering_facts (
    org_id              text             NOT NULL,
    stream_id           text             NOT NULL,
    intervention_id     text             NOT NULL,
    ref                 text             NOT NULL,
    signal              text             NOT NULL,
    phase               text             NOT NULL,
    at                  timestamptz      NOT NULL,
    repo                text             NOT NULL,
    session_id          text,
    number              integer,
    person              text             NOT NULL,
    person_mapped       boolean          NOT NULL,
    period              text             NOT NULL,
    text                text,
    refs                jsonb            NOT NULL,
    rule_intent         text,
    status              text             NOT NULL CHECK (status IN ('pending', 'classified', 'unclassified')),
    unclassified_reason text,
    intent              text,
    went_wrong          text,
    went_wrong_label    text,
    prevention          text,
    confidence          double precision,
    label_source        text             CHECK (label_source IN ('rule', 'model', 'human')),
    model               text,
    model_intent        text,
    model_went_wrong    text,
    model_prevention    text,
    relabeled_at        timestamptz,
    PRIMARY KEY (org_id, stream_id, intervention_id)
);
CREATE UNIQUE INDEX steering_facts_ref ON casebox.steering_facts (org_id, ref);
CREATE INDEX steering_facts_at ON casebox.steering_facts (org_id, at);
CREATE INDEX steering_facts_session ON casebox.steering_facts (org_id, session_id);
CREATE INDEX steering_facts_pr ON casebox.steering_facts (org_id, repo, number);
CREATE INDEX steering_facts_pending ON casebox.steering_facts (org_id, stream_id) WHERE status = 'pending';
