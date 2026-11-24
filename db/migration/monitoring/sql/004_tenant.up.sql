CREATE TABLE measurements_new (
    "tenant_id"             TEXT        NOT NULL DEFAULT 'default',
    "client_id"             TEXT        NOT NULL,
    "timestamp"             DATETIME    NOT NULL,
    "cpu_usage_percent"     REAL        NOT NULL,
    "memory_usage_percent"  REAL        NOT NULL,
    "io_usage_percent"      REAL        NOT NULL,
    "processes"             TEXT,
    "mountpoints"           TEXT,
    "net_lan_in"            INTEGER,
    "net_lan_out"           INTEGER,
    "net_wan_in"            INTEGER,
    "net_wan_out"           INTEGER,
    PRIMARY KEY (tenant_id, client_id, timestamp)
);
INSERT INTO measurements_new (tenant_id, client_id, timestamp, cpu_usage_percent, memory_usage_percent, io_usage_percent, processes, mountpoints, net_lan_in, net_lan_out, net_wan_in, net_wan_out)
    SELECT 'default', client_id, timestamp, cpu_usage_percent, memory_usage_percent, io_usage_percent, processes, mountpoints, net_lan_in, net_lan_out, net_wan_in, net_wan_out FROM measurements;
DROP TABLE measurements;
ALTER TABLE measurements_new RENAME TO measurements;
CREATE INDEX "measurements_timestamp" ON `measurements` ("tenant_id" ASC, "timestamp" ASC);
CREATE INDEX "measurements_client_id" ON `measurements` ("tenant_id" ASC, "client_id" ASC);
