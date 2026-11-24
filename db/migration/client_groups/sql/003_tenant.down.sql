CREATE TABLE client_groups_old (
    id TEXT PRIMARY KEY NOT NULL,
    description TEXT NOT NULL,
    params TEXT NOT NULL,
    allowed_user_groups TEXT NOT NULL DEFAULT '[]'
) WITHOUT ROWID;
INSERT INTO client_groups_old (id, description, params, allowed_user_groups)
    SELECT id, description, params, allowed_user_groups FROM client_groups WHERE tenant_id = 'default';
DROP TABLE client_groups;
ALTER TABLE client_groups_old RENAME TO client_groups;
