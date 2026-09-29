-- Simple self-evolution (docs/specs/simple-evolution.md): the replay loop is gone, and a proposal
-- is one drafted change a person approves, rejects and applies on their own machine.
DROP TABLE IF EXISTS casebox.evaluation_checkpoints;
DROP TABLE IF EXISTS casebox.evaluations;
DROP TABLE IF EXISTS casebox.run_results;
DROP TABLE IF EXISTS casebox.ci_runs;
DROP TABLE IF EXISTS casebox.case_validations;
DROP TABLE IF EXISTS casebox.case_catalog;
DROP TABLE IF EXISTS casebox.blobs;
DROP TABLE IF EXISTS casebox.proposal_candidates;
DROP TABLE IF EXISTS casebox.proposals;
DELETE FROM casebox.jobs WHERE kind NOT IN ('steering.classify', 'steering.pr', 'pattern.cluster', 'entire.fetch', 'gitai.fetch');

-- Why the analysis model found no repository change for a pattern.
ALTER TABLE casebox.patterns ADD COLUMN advisory_note text;

CREATE TABLE casebox.proposals (
    org_id       text        NOT NULL,
    id           text        NOT NULL,
    workspace    text        NOT NULL,
    pattern      text        NOT NULL,
    repo         text        NOT NULL,
    kind         text        NOT NULL,
    title        text        NOT NULL,
    rationale    text        NOT NULL,
    edits        jsonb       NOT NULL,
    preview      jsonb       NOT NULL,
    note         jsonb,
    content_hash text        NOT NULL,
    base_commit  text        NOT NULL,
    status       text        NOT NULL,
    reason       text,
    applied_at   timestamptz,
    applied_mode text,
    outcome      jsonb,
    created_at   timestamptz NOT NULL,
    updated_at   timestamptz NOT NULL,
    PRIMARY KEY (org_id, id)
);
CREATE INDEX proposals_workspace ON casebox.proposals (org_id, workspace, status);
CREATE INDEX proposals_pattern ON casebox.proposals (org_id, pattern);
CREATE INDEX proposals_hash ON casebox.proposals (org_id, content_hash);

-- Workspaces no longer carry an environment recipe or a shared harness.
ALTER TABLE casebox.workspaces DROP COLUMN recipe_status;
ALTER TABLE casebox.workspaces DROP COLUMN shared_harness;
