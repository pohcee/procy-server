CREATE TABLE client_groups_new (
    tenant_id TEXT NOT NULL DEFAULT 'default',
    id TEXT NOT NULL,
    description TEXT NOT NULL,
    params TEXT NOT NULL,
    allowed_user_groups TEXT NOT NULL DEFAULT '[]',
    PRIMARY KEY (tenant_id, id)
) WITHOUT ROWID;
INSERT INTO client_groups_new (tenant_id, id, description, params, allowed_user_groups)
    SELECT 'default', id, description, params, allowed_user_groups FROM client_groups;
DROP TABLE client_groups;
ALTER TABLE client_groups_new RENAME TO client_groups;
