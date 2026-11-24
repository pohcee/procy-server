CREATE TABLE multi_jobs_old (
    jid TEXT PRIMARY KEY NOT NULL,
    started_at DATETIME NOT NULL,
    created_by TEXT NOT NULL,
    details TEXT NOT NULL,
    schedule_id TEXT NULL
) WITHOUT ROWID;
INSERT INTO multi_jobs_old (jid, started_at, created_by, details, schedule_id)
    SELECT jid, started_at, created_by, details, schedule_id FROM multi_jobs WHERE tenant_id = 'default';

CREATE TABLE jobs_old (
    jid TEXT PRIMARY KEY NOT NULL,
    status TEXT NOT NULL,
    started_at DATETIME NOT NULL,
    finished_at DATETIME,
    created_by TEXT NOT NULL,
    client_id TEXT NOT NULL,
    multi_job_id TEXT,
    details TEXT NOT NULL,
    FOREIGN KEY (multi_job_id) REFERENCES multi_jobs(jid)
) WITHOUT ROWID;
INSERT INTO jobs_old (jid, status, started_at, finished_at, created_by, client_id, multi_job_id, details)
    SELECT jid, status, started_at, finished_at, created_by, client_id, multi_job_id, details FROM jobs WHERE tenant_id = 'default';

DROP INDEX idx_jobs_client_id_time;
DROP INDEX idx_jobs_multi_id;
DROP TABLE jobs;
DROP TABLE multi_jobs;
ALTER TABLE multi_jobs_old RENAME TO multi_jobs;
ALTER TABLE jobs_old RENAME TO jobs;

CREATE INDEX idx_jobs_client_id_time
    ON jobs (client_id, finished_at DESC);

CREATE INDEX idx_jobs_multi_id
    ON jobs (multi_job_id);

CREATE TABLE schedules_old (
    id TEXT PRIMARY KEY NOT NULL,
    created_at DATETIME NOT NULL,
    created_by TEXT NOT NULL,
    name TEXT NOT NULL,
    schedule TEXT NOT NULL,
    type TEXT NOT NULL,
    details TEXT NOT NULL
);
INSERT INTO schedules_old (id, created_at, created_by, name, schedule, type, details)
    SELECT id, created_at, created_by, name, schedule, type, details FROM schedules WHERE tenant_id = 'default';
DROP TABLE schedules;
ALTER TABLE schedules_old RENAME TO schedules;
