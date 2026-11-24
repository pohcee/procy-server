-- Authentication: API users and groups, API tokens, API sessions and client authentication credentials.
-- Columns mirror the OSS SQLite schemas (after their tenant migrations) so the importer can copy rows as they are.

-- users / user_groups / groups: what the OSS [database] user provider keeps in its users, groups and group details
-- tables. Passwords are stored as the OSS service hands them over (bcrypt hashes).
CREATE TABLE users (
    tenant_id        text    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    username         text    NOT NULL CHECK (username <> ''),
    password         text    NOT NULL DEFAULT '',
    password_expired boolean NOT NULL DEFAULT false,
    two_fa_send_to   text    NOT NULL DEFAULT '',
    totp_secret      text    NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, username)
);
SELECT rport_enable_rls('users');

CREATE TABLE user_groups (
    tenant_id  text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    username   text NOT NULL,
    group_name text NOT NULL,
    PRIMARY KEY (tenant_id, username, group_name),
    FOREIGN KEY (tenant_id, username) REFERENCES users (tenant_id, username) ON DELETE CASCADE ON UPDATE CASCADE
);
CREATE INDEX user_groups_group ON user_groups (tenant_id, group_name);
SELECT rport_enable_rls('user_groups');

-- permissions, tunnels_restricted and commands_restricted hold the JSON the OSS Go types marshal (text, not
-- queried as JSON).
CREATE TABLE groups (
    tenant_id           text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name                text NOT NULL,
    permissions         text NOT NULL DEFAULT '{}',
    tunnels_restricted  text,
    commands_restricted text,
    PRIMARY KEY (tenant_id, name)
);
SELECT rport_enable_rls('groups');

-- user_logins (0001) follows users: logins are unique across tenants, and the tenant of a login is found without
-- knowing the tenant first. A login taken by another tenant fails the insert or rename with a unique violation on
-- user_logins_pkey.
-- SECURITY DEFINER: the application role may only read user_logins; the function (owned by the schema owner)
-- writes it, and only for the row of users that fired the trigger, which row-level security already limits to the
-- transaction's tenant.
CREATE FUNCTION rport_sync_user_logins() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO user_logins (username, tenant_id) VALUES (NEW.username, NEW.tenant_id);
    ELSIF TG_OP = 'UPDATE' THEN
        IF NEW.username IS DISTINCT FROM OLD.username OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
            DELETE FROM user_logins WHERE username = OLD.username AND tenant_id = OLD.tenant_id;
            INSERT INTO user_logins (username, tenant_id) VALUES (NEW.username, NEW.tenant_id);
        END IF;
    ELSE
        DELETE FROM user_logins WHERE username = OLD.username AND tenant_id = OLD.tenant_id;
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER users_sync_logins
    AFTER INSERT OR UPDATE OF username, tenant_id OR DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION rport_sync_user_logins();

-- api_tokens: OSS api_token migration 004.
CREATE TABLE api_tokens (
    tenant_id  text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    username   text        NOT NULL CHECK (username <> ''),
    prefix     text        NOT NULL CHECK (prefix <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    scope      text,
    token      text        NOT NULL,
    name       text        NOT NULL,
    PRIMARY KEY (tenant_id, username, prefix)
);
CREATE UNIQUE INDEX api_tokens_unique_name ON api_tokens (tenant_id, username, name);
SELECT rport_enable_rls('api_tokens');

-- api_sessions: OSS api_sessions migration 003. session_id stays a global, generated key (it is the session
-- token's id), so it never collides across tenants; every query also filters by tenant_id.
CREATE TABLE api_sessions (
    session_id     bigserial PRIMARY KEY,
    tenant_id      text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    expires_at     timestamptz NOT NULL,
    username       text        NOT NULL,
    last_access_at timestamptz NOT NULL,
    user_agent     text,
    ip_address     text
);
CREATE INDEX api_sessions_tenant_expires ON api_sessions (tenant_id, expires_at);
CREATE INDEX api_sessions_tenant_username ON api_sessions (tenant_id, username);
SELECT rport_enable_rls('api_sessions');

-- clients_auth: client authentication credentials (OSS [server] auth_table). Client ids are unique across tenants
-- (a client connecting doesn't name its tenant).
CREATE TABLE clients_auth (
    tenant_id text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id        text NOT NULL,
    password  text,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT clients_auth_id_unique UNIQUE (id)
);
SELECT rport_enable_rls('clients_auth');
