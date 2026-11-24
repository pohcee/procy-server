ALTER TABLE `auditlog` ADD COLUMN "tenant_id" TEXT NOT NULL DEFAULT 'default';

CREATE INDEX "auditlog_tenant_timestamp" ON `auditlog` (
    "tenant_id" ASC,
    "timestamp" ASC
);

-- 001 indexed columns that don't exist (client_username, client_remote_ip): SQLite took the quoted names as string
-- literals, so these indexes indexed a constant. Index the real columns instead.
DROP INDEX "auditlog_username";
CREATE INDEX "auditlog_username" ON `auditlog` (
    "tenant_id" ASC,
    "username" ASC
);

DROP INDEX "auditlog_remote_ip";
CREATE INDEX "auditlog_remote_ip" ON `auditlog` (
    "tenant_id" ASC,
    "remote_ip" ASC
);
