CREATE TABLE api_sessions_old (
    session_id INTEGER PRIMARY KEY NOT NULL,
    expires_at DATETIME NOT NULL,
    username TEXT NOT NULL,
    last_access_at DATETIME NOT NULL,
    user_agent TEXT,
    ip_address TEXT
);
INSERT INTO api_sessions_old (session_id, expires_at, username, last_access_at, user_agent, ip_address)
    SELECT session_id, expires_at, username, last_access_at, user_agent, ip_address FROM api_sessions WHERE tenant_id = 'default';
DROP INDEX IF EXISTS idx_expires_at_time;
DROP INDEX IF EXISTS idx_tenant_username;
DROP TABLE api_sessions;
ALTER TABLE api_sessions_old RENAME TO api_sessions;

CREATE INDEX idx_expires_at_time
	ON api_sessions (DATETIME(expires_at) DESC);

-- username may not be unique as the user may create new tokens before old ones are deleted/expired
CREATE INDEX idx_username
  ON api_sessions (username);
