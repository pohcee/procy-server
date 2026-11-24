CREATE TABLE api_tokens_new (
    tenant_id TEXT NOT NULL DEFAULT 'default',
    username TEXT NOT NULL CHECK (username != ''),
    prefix TEXT NOT NULL CHECK (prefix != ''),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME,
    scope TEXT,
    token TEXT NOT NULL,
    name TEXT NOT NULL,
    PRIMARY KEY (tenant_id, username, prefix)
) WITHOUT ROWID;
INSERT INTO api_tokens_new (tenant_id, username, prefix, created_at, expires_at, scope, token, name)
    SELECT 'default', username, prefix, created_at, expires_at, scope, token, name FROM api_tokens;
DROP INDEX IF EXISTS api_tokens_unique_name;
DROP TABLE api_tokens;
ALTER TABLE api_tokens_new RENAME TO api_tokens;
CREATE UNIQUE INDEX api_tokens_unique_name
    ON api_tokens (tenant_id, username, name);
