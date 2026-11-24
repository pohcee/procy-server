-- Monitoring measurements (server/monitoring, monitoring.db after 004). A plain table keyed tenant-first; no
-- partitioning or retention tuning yet. processes and mountpoints hold the JSON the client sent, as text.

CREATE TABLE measurements (
    tenant_id            text             NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    client_id            text             NOT NULL,
    timestamp            timestamptz      NOT NULL,
    cpu_usage_percent    double precision NOT NULL,
    memory_usage_percent double precision NOT NULL,
    io_usage_percent     double precision NOT NULL,
    processes            text             NULL,
    mountpoints          text             NULL,
    net_lan_in           bigint           NULL,
    net_lan_out          bigint           NULL,
    net_wan_in           bigint           NULL,
    net_wan_out          bigint           NULL,
    PRIMARY KEY (tenant_id, client_id, timestamp)
);
-- the cleanup deletes by time across a tenant's clients
CREATE INDEX measurements_tenant_timestamp ON measurements (tenant_id, timestamp);
SELECT rport_enable_rls('measurements');
