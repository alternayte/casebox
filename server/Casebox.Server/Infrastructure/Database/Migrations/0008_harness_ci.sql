-- Harness CI (docs/specs/harness-ci.md): baseline evaluations are found by their spec key, and a
-- CI run links a pull request or a nightly baseline to its evaluation.
ALTER TABLE casebox.evaluations
    ADD COLUMN ci_run   text,
    ADD COLUMN spec_key text,
    ADD COLUMN scored   jsonb,
    ADD COLUMN done_at  timestamptz;

CREATE INDEX evaluations_baseline_key ON casebox.evaluations (org_id, spec_key, done_at)
    WHERE purpose = 'baseline' AND status = 'done';

CREATE TABLE casebox.ci_runs (
    org_id        text        NOT NULL,
    id            text        NOT NULL,
    kind          text        NOT NULL,
    workspace     text,
    repo          text        NOT NULL,
    number        integer,
    head_sha      text,
    base_sha      text,
    server_url    text,
    evaluation_id text,
    status        text        NOT NULL,
    message       text,
    request       jsonb       NOT NULL,
    created_by    text        NOT NULL,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (org_id, id)
);

CREATE INDEX ci_runs_pull_request ON casebox.ci_runs (org_id, repo, number, created_at DESC)
    WHERE kind = 'pull_request';
CREATE INDEX ci_runs_evaluation ON casebox.ci_runs (org_id, evaluation_id);

-- The shared harness repository a workspace uses, from casebox.yml harness.shared.
ALTER TABLE casebox.workspaces ADD COLUMN shared_harness text;

-- A ci token runs casebox ci and its inline worker.
ALTER TABLE casebox.api_tokens DROP CONSTRAINT api_tokens_kind_check;
ALTER TABLE casebox.api_tokens ADD CONSTRAINT api_tokens_kind_check CHECK (kind IN ('worker', 'ingest', 'cli', 'ci'));
