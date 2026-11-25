-- OpenID Connect sign-in (standalone; Postgres keeps the same in migration 1700).
-- oidc_logins: pending sign-ins (GET /oidc/login until the callback), by the SHA-256 of the state.
CREATE TABLE oidc_logins (
    state_hash   TEXT PRIMARY KEY,
    nonce        TEXT NOT NULL,
    verifier     TEXT NOT NULL,
    binding_hash TEXT NOT NULL,
    return_to    TEXT NOT NULL DEFAULT '',
    expires_at   DATETIME NOT NULL
) WITHOUT ROWID;
CREATE INDEX oidc_logins_expires ON oidc_logins (expires_at);

-- oidc_tickets: one-time sign-in codes the callback hands to the web UI, by the SHA-256 of the code.
CREATE TABLE oidc_tickets (
    code_hash    TEXT PRIMARY KEY,
    username     TEXT NOT NULL,
    binding_hash TEXT NOT NULL,
    expires_at DATETIME NOT NULL,
    used_at    DATETIME
) WITHOUT ROWID;
CREATE INDEX oidc_tickets_expires ON oidc_tickets (expires_at);

-- oidc_identities: the provider identity (issuer, subject) pinned to a login at its first sign-in.
CREATE TABLE oidc_identities (
    issuer        TEXT NOT NULL,
    subject       TEXT NOT NULL,
    username      TEXT NOT NULL,
    email         TEXT NOT NULL DEFAULT '',
    linked_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_login_at DATETIME,
    PRIMARY KEY (issuer, subject),
    UNIQUE (username, issuer)
) WITHOUT ROWID;
