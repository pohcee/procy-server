CREATE TABLE multi_jobs_new (
    tenant_id TEXT NOT NULL DEFAULT 'default',
    jid TEXT NOT NULL,
    started_at DATETIME NOT NULL,
    created_by TEXT NOT NULL,
    details TEXT NOT NULL,
    schedule_id TEXT NULL,
    PRIMARY KEY (tenant_id, jid)
) WITHOUT ROWID;
INSERT INTO multi_jobs_new (tenant_id, jid, started_at, created_by, details, schedule_id)
    SELECT 'default', jid, started_at, created_by, details, schedule_id FROM multi_jobs;

CREATE TABLE jobs_new (
    tenant_id TEXT NOT NULL DEFAULT 'default',
    jid TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at DATETIME NOT NULL,
    finished_at DATETIME,
    created_by TEXT NOT NULL,
    client_id TEXT NOT NULL,
    multi_job_id TEXT,
    details TEXT NOT NULL,
    PRIMARY KEY (tenant_id, jid),
    FOREIGN KEY (tenant_id, multi_job_id) REFERENCES multi_jobs(tenant_id, jid)
) WITHOUT ROWID;
INSERT INTO jobs_new (tenant_id, jid, status, started_at, finished_at, created_by, client_id, multi_job_id, details)
    SELECT 'default', jid, status, started_at, finished_at, created_by, client_id, multi_job_id, details FROM jobs;

DROP INDEX idx_jobs_client_id_time;
DROP INDEX idx_jobs_multi_id;
DROP TABLE jobs;
DROP TABLE multi_jobs;
ALTER TABLE multi_jobs_new RENAME TO multi_jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE INDEX idx_jobs_client_id_time
    ON jobs (tenant_id, client_id, finished_at DESC);

CREATE INDEX idx_jobs_multi_id
    ON jobs (tenant_id, multi_job_id);

CREATE TABLE schedules_new (
    tenant_id TEXT NOT NULL DEFAULT 'default',
    id TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    created_by TEXT NOT NULL,
    name TEXT NOT NULL,
    schedule TEXT NOT NULL,
    type TEXT NOT NULL,
    details TEXT NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
INSERT INTO schedules_new (tenant_id, id, created_at, created_by, name, schedule, type, details)
    SELECT 'default', id, created_at, created_by, name, schedule, type, details FROM schedules;
DROP TABLE schedules;
ALTER TABLE schedules_new RENAME TO schedules;
