-- The commit a session started from when no hook recorded it: the last commit on its branch at
-- or before its start, found by the CLI. Steering cases start from it.
ALTER TABLE casebox.sessions ADD COLUMN base_commit text;

-- One row per case (projection of the case streams). instruction holds tokens, never an identity;
-- the API masks them. It is scrubbed when its person is erased.
CREATE TABLE casebox.case_catalog (
    org_id              text             NOT NULL,
    id                  text             NOT NULL,
    kind                text             NOT NULL,
    workspace           text             NOT NULL,
    status              text             NOT NULL,
    scope               text             NOT NULL,
    source              text             NOT NULL,
    work_item           text,
    repos               jsonb            NOT NULL,
    rank                integer          NOT NULL,
    recipe_hash         text             NOT NULL,
    harness_hash        text             NOT NULL,
    oracle              text,
    fail_to_pass        integer,
    pass_to_pass        integer,
    seconds             double precision,
    drift               boolean          NOT NULL DEFAULT false,
    weight              double precision NOT NULL DEFAULT 1,
    failure_reason      text,
    failure_detail      text,
    instruction         text,
    person              text,
    signatures          jsonb            NOT NULL DEFAULT '[]',
    assertions          jsonb            NOT NULL DEFAULT '[]',
    judge               jsonb            NOT NULL DEFAULT '[]',
    assertions_approved boolean          NOT NULL DEFAULT false,
    split               text,
    reject_reason       text,
    retired_reason      text,
    mined_at            timestamptz      NOT NULL,
    updated_at          timestamptz      NOT NULL,
    PRIMARY KEY (org_id, id)
);
CREATE INDEX case_catalog_status ON casebox.case_catalog (org_id, workspace, status);

-- Every validation run of a case, passed or failed, for the Cases page.
CREATE TABLE casebox.case_validations (
    org_id       text             NOT NULL,
    case_id      text             NOT NULL,
    event_id     uuid             NOT NULL,
    at           timestamptz      NOT NULL,
    passed       boolean          NOT NULL,
    reason       text,
    detail       text,
    fail_to_pass integer,
    pass_to_pass integer,
    seconds      double precision,
    PRIMARY KEY (org_id, case_id, event_id)
);
