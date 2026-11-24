-- Background work of a cluster: each periodic task runs on one node at a time per tenant, and each scheduled run of
-- a schedule starts once, however many nodes run the schedule's cron.

-- work_leases: the node (holder: node id and a per-process suffix) that runs task name for tenant_id until until.
-- The holder renews it on each run; another node takes it over once it has expired. tenant_id is '' for work that
-- belongs to no tenant (change feed retention). Not tenant data: no row-level security, so a node can take leases for
-- every tenant.
CREATE TABLE work_leases (
    name       text        NOT NULL,
    tenant_id  text        NOT NULL,
    holder     text        NOT NULL,
    until      timestamptz NOT NULL,
    PRIMARY KEY (name, tenant_id)
);

-- schedule_runs: one row per scheduled run that a node started. The node inserts it before starting the run and
-- starts it only if the insert added the row, so a schedule fires once per fire time.
CREATE TABLE schedule_runs (
    tenant_id   text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    schedule_id text        NOT NULL,
    fire_time   timestamptz NOT NULL,
    holder      text        NOT NULL,
    claimed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, schedule_id, fire_time)
);
CREATE INDEX schedule_runs_claimed_at ON schedule_runs (claimed_at);
SELECT rport_enable_rls('schedule_runs');

-- Built-in library entries are synced by one node at a time (a lease), and this keeps two of the same built-in from
-- ever being created in a tenant.
CREATE UNIQUE INDEX library_entries_unique_builtin_key ON library_entries (tenant_id, builtin_key) WHERE builtin_key IS NOT NULL;
DROP INDEX library_entries_builtin_key;
