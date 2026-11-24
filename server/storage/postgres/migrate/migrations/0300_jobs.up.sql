-- Jobs, multi-client jobs and schedules (OSS jobs.db after 004_tenant). details holds the JSON the OSS Go code
-- marshals, as text, byte for byte; queries that read a JSON field cast it to jsonb.

CREATE TABLE multi_jobs (
    tenant_id   text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    jid         text        NOT NULL,
    started_at  timestamptz NOT NULL,
    created_by  text        NOT NULL,
    details     text        NOT NULL,
    schedule_id text        NULL,
    -- the run's error, also in details: the schedule list reads it here, since a command with a NUL (\u0000 in
    -- details) can't be cast to json(b). NULs are dropped from this copy.
    run_error   text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, jid)
);
CREATE INDEX multi_jobs_started_at ON multi_jobs (tenant_id, started_at DESC);
CREATE INDEX multi_jobs_schedule ON multi_jobs (tenant_id, schedule_id, started_at DESC);
SELECT rport_enable_rls('multi_jobs');

-- The reference to multi_jobs is checked at commit: cleanup deletes a multi-job before its jobs, in one
-- transaction, as the SQLite implementation does (where it isn't enforced).
CREATE TABLE jobs (
    tenant_id    text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    jid          text        NOT NULL,
    status       text        NOT NULL,
    started_at   timestamptz NOT NULL,
    finished_at  timestamptz NULL,
    created_by   text        NOT NULL,
    client_id    text        NOT NULL,
    multi_job_id text        NULL,
    details      text        NOT NULL,
    PRIMARY KEY (tenant_id, jid),
    FOREIGN KEY (tenant_id, multi_job_id) REFERENCES multi_jobs (tenant_id, jid) DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX idx_jobs_client_id_time ON jobs (tenant_id, client_id, finished_at DESC);
CREATE INDEX idx_jobs_multi_id ON jobs (tenant_id, multi_job_id);
CREATE INDEX jobs_started_at ON jobs (tenant_id, started_at DESC);
SELECT rport_enable_rls('jobs');

CREATE TABLE schedules (
    tenant_id  text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id         text        NOT NULL,
    created_at timestamptz NOT NULL,
    created_by text        NOT NULL,
    name       text        NOT NULL,
    schedule   text        NOT NULL,
    type       text        NOT NULL,
    details    text        NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
SELECT rport_enable_rls('schedules');
