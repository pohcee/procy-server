CREATE TABLE "values_new"
(
    "id"             INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "tenant_id"      TEXT    NOT NULL DEFAULT 'default',
    "client_id"      text    NOT NULL DEFAULT 0,
    "required_group" TEXT,
    "created_at"     DATE    NOT NULL,
    "created_by"     TEXT    NOT NULL,
    "updated_at"     DATE    NOT NULL,
    "updated_by"     TEXT,
    "key"            TEXT    NOT NULL,
    "value"          TEXT    NOT NULL,
    "type"           TEXT    NOT NULL
);
INSERT INTO "values_new" ("id", "tenant_id", "client_id", "required_group", "created_at", "created_by", "updated_at", "updated_by", "key", "value", "type")
    SELECT "id", 'default', "client_id", "required_group", "created_at", "created_by", "updated_at", "updated_by", "key", "value", "type" FROM "values";
-- keep the id sequence, so ids of deleted values are never handed out again
DELETE FROM `sqlite_sequence` WHERE `name` = 'values_new';
INSERT INTO `sqlite_sequence` (`name`, `seq`) SELECT 'values_new', `seq` FROM `sqlite_sequence` WHERE `name` = 'values';
DROP TABLE "values";
ALTER TABLE "values_new" RENAME TO "values";
CREATE INDEX "client_id" ON `values` ("tenant_id" ASC, "client_id" ASC);
CREATE INDEX "key" ON `values` ("tenant_id" ASC, "key" ASC);
CREATE UNIQUE INDEX "unique_client_id_key" ON `values` ("tenant_id" ASC, "client_id" ASC, "key" ASC);

CREATE TABLE "status_new"
(
    "id"        INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "tenant_id" TEXT    NOT NULL DEFAULT 'default',
    "db_status" text    NOT NULL,
    "enc_check" TEXT,
    "dec_check" TEXT
);
-- one status row per tenant: the oldest, which is the one the server has been reading
INSERT INTO "status_new" ("id", "tenant_id", "db_status", "enc_check", "dec_check")
    SELECT "id", 'default', "db_status", "enc_check", "dec_check" FROM "status" ORDER BY "id" LIMIT 1;
DELETE FROM `sqlite_sequence` WHERE `name` = 'status_new';
INSERT INTO `sqlite_sequence` (`name`, `seq`) SELECT 'status_new', `seq` FROM `sqlite_sequence` WHERE `name` = 'status';
DROP TABLE "status";
ALTER TABLE "status_new" RENAME TO "status";
CREATE UNIQUE INDEX "status__unique_tenant" ON `status` ("tenant_id" ASC);
