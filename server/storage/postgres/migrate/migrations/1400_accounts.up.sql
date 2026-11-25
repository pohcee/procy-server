-- Membership in several organizations (phase 6): a login is one account with one credential, and a member of one
-- or more tenants.
--
-- accounts holds the credential of a login (password hash, whether it expired, TOTP secret) once for all its
-- tenants. The users rows stay per tenant (a membership: groups and the 2FA delivery address of that organization);
-- their password, password_expired and totp_secret columns are kept empty from here on. user_logins becomes the
-- membership index: one row per (login, tenant).
--
-- Writers of users (the user store, the importer) keep working unchanged on insert: a trigger creates the account
-- from the new row's credential. A users row for a login that already has an account in another tenant is a new
-- membership, allowed only when the transaction says so (rport.join_account = the login, set by the invitation
-- code path); otherwise it fails as before ("already taken").
CREATE TABLE accounts (
    username         text        PRIMARY KEY CHECK (username <> ''),
    password         text        NOT NULL DEFAULT '',
    password_expired boolean     NOT NULL DEFAULT false,
    totp_secret      text        NOT NULL DEFAULT '',
    -- the organization the last login or switch picked: the one a login naming none goes to
    last_tenant_id   text        REFERENCES tenants (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE user_logins DROP CONSTRAINT user_logins_pkey;
ALTER TABLE user_logins ADD PRIMARY KEY (username, tenant_id);

-- Existing users: one tenant per login so far, so each becomes an account. Row-level security is lifted for the
-- owner for the copy only (FORCE applies it to the owner as well).
ALTER TABLE users NO FORCE ROW LEVEL SECURITY;
INSERT INTO accounts (username, password, password_expired, totp_secret)
    SELECT username, password, password_expired, totp_secret FROM users;
UPDATE users SET password = '', password_expired = false, totp_secret = '';
ALTER TABLE users FORCE ROW LEVEL SECURITY;

-- rport_account_member_elsewhere: the login is a member of a tenant other than p_tenant.
-- Called by the trigger functions below, which run as the owner.
CREATE FUNCTION rport_account_member_elsewhere(p_username text, p_tenant text) RETURNS boolean
    LANGUAGE sql STABLE SET search_path = public, pg_temp AS $$
    SELECT EXISTS (SELECT 1 FROM user_logins WHERE username = p_username AND tenant_id <> p_tenant)
$$;

-- Before a users row is written: create, join or rename its account, and keep the credential columns empty.
CREATE FUNCTION rport_users_account() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM accounts WHERE username = NEW.username) THEN
            IF current_setting('rport.join_account', true) = NEW.username THEN
                RAISE EXCEPTION 'login % has no account to join', NEW.username USING ERRCODE = 'no_data_found';
            END IF;
            INSERT INTO accounts (username, password, password_expired, totp_secret)
                VALUES (NEW.username, NEW.password, NEW.password_expired, NEW.totp_secret);
        ELSIF EXISTS (SELECT 1 FROM user_logins WHERE username = NEW.username AND tenant_id = NEW.tenant_id) THEN
            -- the same tenant already has the user: let the primary key of users decide (error or ON CONFLICT)
            NULL;
        ELSIF current_setting('rport.join_account', true) IS DISTINCT FROM NEW.username THEN
            RAISE EXCEPTION 'login % belongs to another organization', NEW.username
                USING ERRCODE = 'unique_violation', CONSTRAINT = 'accounts_pkey';
        END IF;
    ELSIF NEW.username IS DISTINCT FROM OLD.username THEN
        -- a rename renames the account, which only a login of this tenant alone may do
        IF rport_account_member_elsewhere(OLD.username, OLD.tenant_id) THEN
            RAISE EXCEPTION 'login % belongs to other organizations too: it cannot be renamed', OLD.username
                USING ERRCODE = 'check_violation', CONSTRAINT = 'accounts_shared';
        END IF;
        IF EXISTS (SELECT 1 FROM accounts WHERE username = NEW.username) THEN
            RAISE EXCEPTION 'login % is taken', NEW.username USING ERRCODE = 'unique_violation', CONSTRAINT = 'accounts_pkey';
        END IF;
        UPDATE accounts SET username = NEW.username WHERE username = OLD.username;
    END IF;
    IF NEW.password <> '' OR NEW.password_expired OR NEW.totp_secret <> '' THEN
        IF TG_OP = 'UPDATE' THEN
            -- a direct write of the credential columns goes to the account (the user store uses rport_set_account,
            -- which can also clear password_expired)
            UPDATE accounts SET
                password = CASE WHEN NEW.password <> '' THEN NEW.password ELSE password END,
                password_expired = password_expired OR NEW.password_expired,
                totp_secret = CASE WHEN NEW.totp_secret <> '' THEN NEW.totp_secret ELSE totp_secret END
            WHERE username = NEW.username;
        END IF;
        NEW.password := '';
        NEW.password_expired := false;
        NEW.totp_secret := '';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER users_account
    BEFORE INSERT OR UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION rport_users_account();

-- After the last membership of a login is removed, its account goes too (a deleted user is gone, as in standalone).
CREATE FUNCTION rport_users_account_cleanup() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    IF NOT rport_account_member_elsewhere(OLD.username, OLD.tenant_id) THEN
        DELETE FROM accounts WHERE username = OLD.username;
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER users_account_cleanup
    AFTER DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION rport_users_account_cleanup();

-- rport_app_caller: the connected login may write users (the application role), not only call functions (the
-- device role, which has no table privileges). The account functions below are executable only by the roles
-- "rportd migrate --grant-to" names (EXECUTE revoked from PUBLIC); this check is defence in depth.
CREATE FUNCTION rport_app_caller() RETURNS void LANGUAGE plpgsql STABLE AS $$
BEGIN
    IF NOT has_table_privilege(session_user, 'public.users', 'UPDATE') THEN
        RAISE EXCEPTION 'accounts are changed only by the application role' USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

-- rport_set_account changes the credential of a login that is a member of the transaction's tenant (row-level
-- security on users limits the check to it, also for the owner). NULL leaves a field as it is. Returns false when
-- the login isn't a member there.
CREATE FUNCTION rport_set_account(p_username text, p_password text, p_password_expired boolean, p_totp_secret text)
    RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_app_caller();
    IF NOT EXISTS (SELECT 1 FROM users WHERE username = p_username
                   AND tenant_id = current_setting('rport.tenant_id', true)) THEN
        RETURN false;
    END IF;
    UPDATE accounts SET
        password = coalesce(p_password, password),
        password_expired = coalesce(p_password_expired, password_expired),
        totp_secret = coalesce(p_totp_secret, totp_secret)
    WHERE username = p_username;
    RETURN FOUND;
END
$$;

-- rport_remember_tenant makes the transaction's tenant the one the next login of p_username that names no
-- organization goes to, if the login is a member there.
CREATE FUNCTION rport_remember_tenant(p_username text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_app_caller();
    IF NOT EXISTS (SELECT 1 FROM users WHERE username = p_username
                   AND tenant_id = current_setting('rport.tenant_id', true)) THEN
        RETURN false;
    END IF;
    UPDATE accounts SET last_tenant_id = current_setting('rport.tenant_id', true) WHERE username = p_username;
    RETURN FOUND;
END
$$;

-- api_token_logins: which tenant an API token (login + prefix) belongs to, so a request authenticating with it
-- resolves to that tenant without knowing it first (API tokens are bound to one organization). Maintained by a
-- trigger on api_tokens, as user_logins follows users.
CREATE TABLE api_token_logins (
    username  text NOT NULL,
    prefix    text NOT NULL,
    tenant_id text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    PRIMARY KEY (username, prefix, tenant_id)
);
CREATE INDEX api_token_logins_tenant ON api_token_logins (tenant_id);

ALTER TABLE api_tokens NO FORCE ROW LEVEL SECURITY;
INSERT INTO api_token_logins (username, prefix, tenant_id) SELECT username, prefix, tenant_id FROM api_tokens;
ALTER TABLE api_tokens FORCE ROW LEVEL SECURITY;

CREATE FUNCTION rport_sync_api_token_logins() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        DELETE FROM api_token_logins WHERE username = OLD.username AND prefix = OLD.prefix AND tenant_id = OLD.tenant_id;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        INSERT INTO api_token_logins (username, prefix, tenant_id) VALUES (NEW.username, NEW.prefix, NEW.tenant_id)
            ON CONFLICT DO NOTHING;
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER api_tokens_sync_logins
    AFTER INSERT OR UPDATE OF username, prefix, tenant_id OR DELETE ON api_tokens
    FOR EACH ROW EXECUTE FUNCTION rport_sync_api_token_logins();

REVOKE EXECUTE ON FUNCTION rport_set_account(text, text, boolean, text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_remember_tenant(text) FROM PUBLIC;
