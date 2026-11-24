DELETE FROM `auditlog` WHERE "tenant_id" != 'default';

DROP INDEX "auditlog_tenant_timestamp";
DROP INDEX "auditlog_username";
DROP INDEX "auditlog_remote_ip";

ALTER TABLE `auditlog` DROP COLUMN "tenant_id";

-- restore the 001 indexes as they were (they name columns that don't exist; see 002_tenant.up.sql)
CREATE INDEX "auditlog_username" ON `auditlog` (
    "client_username" ASC
);

CREATE INDEX "auditlog_remote_ip" ON `auditlog` (
    "client_remote_ip" ASC
);
