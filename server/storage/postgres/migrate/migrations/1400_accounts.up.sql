-- Membership in several organizations (phase 6): a login is one account with one credential, and a member of one
-- or more tenants.
--
-- accounts holds what belongs to the person behind a login, once for all its tenants: the password hash, whether it
-- expired, the TOTP secret and the 2FA delivery address (an organization's admin must not be able to redirect the
-- 2FA codes of a login other organizations share). The users rows stay per tenant: a membership, with its groups.
-- Their password, password_expired, totp_secret and two_fa_send_to columns are kept empty from here on.
-- user_logins becomes the membership index: one row per (login, tenant).
--
-- Writers of users (the user store, the importer) keep working unchanged on insert: a trigger creates the account
-- from the new row. A users row for a login that already has an account in another tenant is a new membership,
-- allowed only when the transaction says so (rport.join_account = the login, set when the account's owner accepts
-- an invitation); otherwise it fails as before ("already taken").
CREATE TABLE accounts (
    username         text        PRIMARY KEY CHECK (username <> ''),
    password         text        NOT NULL DEFAULT '',
    password_expired boolean     NOT NULL DEFAULT false,
    totp_secret      text        NOT NULL DEFAULT '',
    two_fa_send_to   text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now()
);
-- The application role sees the accounts of the transaction's tenant's members only (not FORCEd: the owner, and so
-- the SECURITY DEFINER functions and triggers below, see every account).
ALTER TABLE accounts ENABLE ROW LEVEL SECURITY;
CREATE POLICY member_of_tenant ON accounts USING (EXISTS (
    SELECT 1 FROM user_logins l WHERE l.username = accounts.username
      AND l.tenant_id = current_setting('rport.tenant_id', true)));

ALTER TABLE user_logins DROP CONSTRAINT user_logins_pkey;
ALTER TABLE user_logins ADD PRIMARY KEY (username, tenant_id);

-- Existing users: one tenant per login so far, so each becomes an account. Row-level security is lifted for the
-- owner for the copy only (FORCE applies it to the owner as well).
ALTER TABLE users NO FORCE ROW LEVEL SECURITY;
INSERT INTO accounts (username, password, password_expired, totp_secret, two_fa_send_to)
    SELECT username, password, password_expired, totp_secret, two_fa_send_to FROM users;
UPDATE users SET password = '', password_expired = false, totp_secret = '', two_fa_send_to = '';
ALTER TABLE users FORCE ROW LEVEL SECURITY;

-- membership_invitations: an organization's invitation of an existing account, pending until the account's owner
-- accepts it (or declines; the organization may withdraw it). Tenant-scoped for the application role; the owner
-- (rport_invitations_of) lists a login's invitations across tenants.
CREATE TABLE membership_invitations (
    tenant_id  text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    username   text        NOT NULL,
    groups     text        NOT NULL DEFAULT '[]',
    invited_by text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, username)
);
CREATE INDEX membership_invitations_username ON membership_invitations (username);
ALTER TABLE membership_invitations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON membership_invitations
    USING (tenant_id = current_setting('rport.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('rport.tenant_id', true));

-- rport_account_member_elsewhere: the login is a member of a tenant other than p_tenant.
-- Called by the trigger functions below, which run as the owner.
CREATE FUNCTION rport_account_member_elsewhere(p_username text, p_tenant text) RETURNS boolean
    LANGUAGE sql STABLE SET search_path = public, pg_temp AS $$
    SELECT EXISTS (SELECT 1 FROM user_logins WHERE username = p_username AND tenant_id <> p_tenant)
$$;

-- Before a users row is written: create, join or rename its account, and keep the account's columns empty. The
-- account row is locked first, so a join, a rename and the removal of a last membership don't interleave.
CREATE FUNCTION rport_users_account() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM 1 FROM accounts WHERE username = NEW.username FOR UPDATE;
        IF NOT FOUND THEN
            IF current_setting('rport.join_account', true) = NEW.username THEN
                RAISE EXCEPTION 'login % has no account to join', NEW.username USING ERRCODE = 'no_data_found';
            END IF;
            INSERT INTO accounts (username, password, password_expired, totp_secret, two_fa_send_to)
                VALUES (NEW.username, NEW.password, NEW.password_expired, NEW.totp_secret, NEW.two_fa_send_to);
        ELSIF EXISTS (SELECT 1 FROM user_logins WHERE username = NEW.username AND tenant_id = NEW.tenant_id) THEN
            -- the same tenant already has the user: let the primary key of users decide (error or ON CONFLICT)
            NULL;
        ELSIF current_setting('rport.join_account', true) IS DISTINCT FROM NEW.username THEN
            RAISE EXCEPTION 'login % belongs to another organization', NEW.username
                USING ERRCODE = 'unique_violation', CONSTRAINT = 'accounts_pkey';
        END IF;
    ELSIF NEW.username IS DISTINCT FROM OLD.username THEN
        -- a rename renames the account, which only a login of this tenant alone may do
        PERFORM 1 FROM accounts WHERE username = OLD.username FOR UPDATE;
        IF rport_account_member_elsewhere(OLD.username, OLD.tenant_id) THEN
            RAISE EXCEPTION 'login % belongs to other organizations too: it cannot be renamed', OLD.username
                USING ERRCODE = 'check_violation', CONSTRAINT = 'accounts_shared';
        END IF;
        IF EXISTS (SELECT 1 FROM accounts WHERE username = NEW.username) THEN
            RAISE EXCEPTION 'login % is taken', NEW.username USING ERRCODE = 'unique_violation', CONSTRAINT = 'accounts_pkey';
        END IF;
        UPDATE accounts SET username = NEW.username WHERE username = OLD.username;
        -- invitations of the old login were for its owner, not for whoever takes the name next
        DELETE FROM membership_invitations WHERE username = OLD.username;
    END IF;
    IF NEW.password <> '' OR NEW.password_expired OR NEW.totp_secret <> '' OR NEW.two_fa_send_to <> '' THEN
        IF TG_OP = 'UPDATE' THEN
            -- a direct write of the account's columns goes to the account (the user store uses rport_set_account,
            -- which can also clear password_expired)
            UPDATE accounts SET
                password = CASE WHEN NEW.password <> '' THEN NEW.password ELSE password END,
                password_expired = password_expired OR NEW.password_expired,
                totp_secret = CASE WHEN NEW.totp_secret <> '' THEN NEW.totp_secret ELSE totp_secret END,
                two_fa_send_to = CASE WHEN NEW.two_fa_send_to <> '' THEN NEW.two_fa_send_to ELSE two_fa_send_to END
            WHERE username = NEW.username;
        END IF;
        NEW.password := '';
        NEW.password_expired := false;
        NEW.totp_secret := '';
        NEW.two_fa_send_to := '';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER users_account
    BEFORE INSERT OR UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION rport_users_account();

-- After the last membership of a login is removed, its account and pending invitations go too (a deleted user is
-- gone, as in standalone, and the name is free for someone else).
CREATE FUNCTION rport_users_account_cleanup() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM 1 FROM accounts WHERE username = OLD.username FOR UPDATE;
    IF NOT rport_account_member_elsewhere(OLD.username, OLD.tenant_id) THEN
        DELETE FROM accounts WHERE username = OLD.username;
        DELETE FROM membership_invitations WHERE username = OLD.username;
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

-- rport_set_account changes the account of a login that is a member of the transaction's tenant (row-level
-- security on users limits the check to it, also for the owner). NULL leaves a field as it is. Returns false when
-- the login isn't a member there.
CREATE FUNCTION rport_set_account(p_username text, p_password text, p_password_expired boolean, p_totp_secret text,
                                  p_two_fa_send_to text)
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
        totp_secret = coalesce(p_totp_secret, totp_secret),
        two_fa_send_to = coalesce(p_two_fa_send_to, two_fa_send_to)
    WHERE username = p_username;
    RETURN FOUND;
END
$$;

-- rport_account_exists: the login has an account (in any tenant); the application role can't see accounts of
-- other tenants' members.
CREATE FUNCTION rport_account_exists(p_username text) RETURNS boolean
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_app_caller();
    RETURN EXISTS (SELECT 1 FROM accounts WHERE username = p_username);
END
$$;

-- rport_invite records the transaction's tenant's invitation of the existing account p_username (replacing an
-- earlier one). Returns 'invited', 'member' (it is a member there already) or 'no_account'. It locks the account
-- row, so the account can't go away in between.
CREATE FUNCTION rport_invite(p_username text, p_groups text, p_invited_by text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    t text := current_setting('rport.tenant_id', true);
BEGIN
    PERFORM rport_app_caller();
    PERFORM 1 FROM accounts WHERE username = p_username FOR UPDATE;
    IF NOT FOUND THEN
        RETURN 'no_account';
    END IF;
    IF EXISTS (SELECT 1 FROM user_logins WHERE username = p_username AND tenant_id = t) THEN
        RETURN 'member';
    END IF;
    INSERT INTO membership_invitations (tenant_id, username, groups, invited_by) VALUES (t, p_username, p_groups, p_invited_by)
        ON CONFLICT (tenant_id, username) DO UPDATE SET groups = EXCLUDED.groups, invited_by = EXCLUDED.invited_by,
            created_at = now();
    RETURN 'invited';
END
$$;

-- rport_invitations_of lists the pending invitations of p_username from active tenants.
CREATE FUNCTION rport_invitations_of(p_username text)
    RETURNS TABLE (id text, name text, invited_by text, created_at timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_app_caller();
    RETURN QUERY SELECT t.id, t.name, i.invited_by, i.created_at
        FROM membership_invitations i JOIN tenants t ON t.id = i.tenant_id
        WHERE i.username = p_username AND t.status = 'active'
        ORDER BY t.name COLLATE "C", t.id COLLATE "C";
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

REVOKE EXECUTE ON FUNCTION rport_set_account(text, text, boolean, text, text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_account_exists(text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_invite(text, text, text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_invitations_of(text) FROM PUBLIC;
