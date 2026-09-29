-- Per-run results: tests, assertions, judge answers and usage (SDD section 10, plain tables). A
-- row is written when the run job answers and completed when its verification answers.
CREATE TABLE casebox.run_results (
    org_id             text             NOT NULL,
    evaluation_id      text             NOT NULL,
    run_id             text             NOT NULL,
    case_id            text             NOT NULL,
    side               text             NOT NULL,
    repeat             integer          NOT NULL,
    status             text             NOT NULL CHECK (status IN ('ran', 'completed', 'failed')),
    passed             boolean,
    applied            boolean,
    usage              jsonb,
    cost_usd           numeric(12, 6)   NOT NULL DEFAULT 0,
    seconds            double precision,
    turns              integer,
    tool_calls         integer,
    model              text,
    timed_out          boolean          NOT NULL DEFAULT false,
    token_cap_exceeded boolean          NOT NULL DEFAULT false,
    process_checks     jsonb,
    harness_hash       text,
    diff_blob          text,
    log_blob           text,
    trace_blob         text,
    tests              jsonb,
    failed_tests       jsonb,
    assertions         jsonb,
    judge              jsonb,
    reason             text,
    created_at         timestamptz      NOT NULL,
    completed_at       timestamptz,
    PRIMARY KEY (org_id, evaluation_id, run_id)
);
CREATE INDEX run_results_case ON casebox.run_results (org_id, case_id, status);
CREATE INDEX run_results_month ON casebox.run_results (org_id, created_at);

-- The evaluation list and each evaluation's state (projection of the evaluation streams).
CREATE TABLE casebox.evaluations (
    org_id         text           NOT NULL,
    id             text           NOT NULL,
    workspace      text           NOT NULL,
    split          text           NOT NULL,
    purpose        text           NOT NULL,
    status         text           NOT NULL,
    change         text           NOT NULL,
    baseline       jsonb          NOT NULL,
    candidate      jsonb          NOT NULL,
    repeats        integer        NOT NULL,
    delta          double precision NOT NULL,
    cap_usd        numeric(12, 2) NOT NULL,
    estimate       jsonb          NOT NULL,
    mutable_model  boolean        NOT NULL,
    cases          integer        NOT NULL,
    spent_usd      numeric(12, 6) NOT NULL DEFAULT 0,
    runs_completed integer        NOT NULL DEFAULT 0,
    runs_failed    integer        NOT NULL DEFAULT 0,
    verdict        jsonb,
    reason         text,
    created_at     timestamptz    NOT NULL,
    updated_at     timestamptz    NOT NULL,
    PRIMARY KEY (org_id, id)
);

CREATE TABLE casebox.evaluation_checkpoints (
    org_id        text             NOT NULL,
    evaluation_id text             NOT NULL,
    round         integer          NOT NULL,
    level         double precision NOT NULL,
    cases         integer          NOT NULL,
    delta         double precision NOT NULL,
    lower         double precision NOT NULL,
    upper         double precision NOT NULL,
    verdict       text             NOT NULL,
    at            timestamptz      NOT NULL,
    PRIMARY KEY (org_id, evaluation_id, round)
);
