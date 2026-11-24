CREATE TABLE "values_old"
(
    "id"             INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
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
INSERT INTO "values_old" ("id", "client_id", "required_group", "created_at", "created_by", "updated_at", "updated_by", "key", "value", "type")
    SELECT "id", "client_id", "required_group", "created_at", "created_by", "updated_at", "updated_by", "key", "value", "type" FROM "values" WHERE "tenant_id" = 'default';
DELETE FROM `sqlite_sequence` WHERE `name` = 'values_old';
INSERT INTO `sqlite_sequence` (`name`, `seq`) SELECT 'values_old', `seq` FROM `sqlite_sequence` WHERE `name` = 'values';
DROP TABLE "values";
ALTER TABLE "values_old" RENAME TO "values";
CREATE INDEX "client_id" ON `values` ("client_id" ASC);
CREATE INDEX "key" ON `values` ("key" ASC);
CREATE UNIQUE INDEX "unique_client_id_key" ON `values` ("client_id" ASC, "key" ASC);

CREATE TABLE "status_old"
(
    "id"        INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "db_status" text    NOT NULL,
    "enc_check" TEXT,
    "dec_check" TEXT
);
INSERT INTO "status_old" ("id", "db_status", "enc_check", "dec_check")
    SELECT "id", "db_status", "enc_check", "dec_check" FROM "status" WHERE "tenant_id" = 'default';
DELETE FROM `sqlite_sequence` WHERE `name` = 'status_old';
INSERT INTO `sqlite_sequence` (`name`, `seq`) SELECT 'status_old', `seq` FROM `sqlite_sequence` WHERE `name` = 'status';
DROP TABLE "status";
ALTER TABLE "status_old" RENAME TO "status";
