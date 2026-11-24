CREATE TABLE api_tokens_old (
    username TEXT NOT NULL CHECK (username != ''),
    prefix TEXT NOT NULL CHECK (prefix != ''),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME,
    scope TEXT,
    token TEXT NOT NULL,
    name TEXT NOT NULL,
    PRIMARY KEY (username, prefix)
) WITHOUT ROWID;
INSERT INTO api_tokens_old (username, prefix, created_at, expires_at, scope, token, name)
    SELECT username, prefix, created_at, expires_at, scope, token, name FROM api_tokens WHERE tenant_id = 'default';
DROP INDEX IF EXISTS api_tokens_unique_name;
DROP TABLE api_tokens;
ALTER TABLE api_tokens_old RENAME TO api_tokens;
CREATE UNIQUE INDEX api_tokens_unique_name
    ON api_tokens (username, name);
