-- Self-service signup and the deletion of organizations ([signup]; billing design § Self-service signup).
-- Both tables belong to the system tenant ('_system'): they are written by the server's signup code inside system
-- tenant transactions, never by organizations, and read by platform operators.

-- signups: one row per accepted signup request. The rate limits per IP, email domain and email count these rows, so
-- they hold on every node of a cluster. token_hash is the SHA-256 of the verification token (the token itself is
-- only in the email); password_hash is the bcrypt hash of the password the user chose, cleared once the signup is
-- completed, denied or expired.
CREATE TABLE signups (
    tenant_id       text        NOT NULL DEFAULT '_system' REFERENCES tenants (id) ON DELETE CASCADE CHECK (tenant_id = '_system'),
    id              text        NOT NULL,
    email           text        NOT NULL,
    domain          text        NOT NULL,
    remote_ip       text        NOT NULL DEFAULT '',
    org_name        text        NOT NULL DEFAULT '',
    password_hash   text        NOT NULL DEFAULT '',
    token_hash      text        NULL,
    -- pending: link sent; existing: the email is already an account (a notice was sent, no link); verify_email: a
    -- signed-in account confirming its email (link sent); verified: that link was used; awaiting_approval:
    -- verified, held for an operator; creating: an operator's approval is creating the organization; completed;
    -- denied; expired
    status          text        NOT NULL CHECK (status IN ('pending', 'existing', 'verify_email', 'verified', 'awaiting_approval', 'creating', 'completed',
                    'denied', 'expired')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    verified_at     timestamptz NULL,
    decided_at      timestamptz NULL,
    decided_by      text        NOT NULL DEFAULT '',
    reason          text        NOT NULL DEFAULT '',
    organization_id text        NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE UNIQUE INDEX signups_token ON signups (token_hash) WHERE token_hash IS NOT NULL;
CREATE INDEX signups_created ON signups (tenant_id, created_at DESC);
CREATE INDEX signups_ip ON signups (tenant_id, remote_ip, created_at);
CREATE INDEX signups_domain ON signups (tenant_id, domain, created_at);
CREATE INDEX signups_email ON signups (tenant_id, email, created_at);
SELECT rport_enable_rls('signups');

-- org_deletions: deletion requests. A pending one holds the organization suspended until delete_after; then the
-- upkeep deletes the tenant (rport_signup_delete_tenant) and marks it done. The row outlives the tenant (no foreign
-- key): it is the record that the organization existed and was deleted. cancel_token_hash is the SHA-256 of the
-- token of the cancel link mailed to the organization's administrators, who can't log in while it is suspended.
CREATE TABLE org_deletions (
    tenant_id         text        NOT NULL DEFAULT '_system' REFERENCES tenants (id) ON DELETE CASCADE CHECK (tenant_id = '_system'),
    id                bigserial,
    organization_id   text        NOT NULL,
    organization_name text        NOT NULL DEFAULT '',
    -- pending: held; deleting: claimed by the upkeep or an expedite (the vault key may be shredded already: no
    -- cancel, no reactivation); canceled; done
    state             text        NOT NULL CHECK (state IN ('pending', 'deleting', 'canceled', 'done')),
    requested_at      timestamptz NOT NULL DEFAULT now(),
    requested_by      text        NOT NULL,
    reason            text        NOT NULL DEFAULT '',
    delete_after      timestamptz NOT NULL,
    cancel_token_hash text        NULL,
    -- the status the organization had before the request: a cancel restores it
    previous_status   text        NOT NULL DEFAULT 'active',
    finished_at       timestamptz NULL,
    finished_by       text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);
CREATE UNIQUE INDEX org_deletions_one_pending ON org_deletions (organization_id) WHERE state IN ('pending', 'deleting');
CREATE UNIQUE INDEX org_deletions_cancel_token ON org_deletions (cancel_token_hash) WHERE cancel_token_hash IS NOT NULL;
CREATE INDEX org_deletions_due ON org_deletions (tenant_id, state, delete_after);
SELECT rport_enable_rls('org_deletions');

-- deleted_tenant_ids: ids of deleted organizations, never given out again (a new organization with an old id
-- could be mistaken for the old one in exports, invoices or audit logs). Not tenant data: no row-level security;
-- the application role may only read it.
CREATE TABLE deleted_tenant_ids (
    id         text PRIMARY KEY,
    deleted_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION rport_refuse_deleted_tenant_id() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM deleted_tenant_ids WHERE id = NEW.id) THEN
        RAISE EXCEPTION 'the id % belonged to a deleted organization', NEW.id USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER tenants_not_deleted_ids BEFORE INSERT ON tenants
    FOR EACH ROW EXECUTE FUNCTION rport_refuse_deleted_tenant_id();

-- An organization whose deletion is under way (state deleting: its vault key may be gone) can't be reactivated.
-- The operator functions change tenants inside system tenant transactions, where org_deletions is visible.
CREATE FUNCTION rport_refuse_reactivating_deleted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'active' AND OLD.status <> 'active' AND EXISTS (
        SELECT 1 FROM org_deletions WHERE tenant_id = '_system' AND organization_id = NEW.id AND state = 'deleting') THEN
        RAISE EXCEPTION 'organization % is being deleted', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER tenants_not_reactivated_while_deleting BEFORE UPDATE OF status ON tenants
    FOR EACH ROW EXECUTE FUNCTION rport_refuse_reactivating_deleted();

-- rport_signup_delete_tenant deletes an organization whose deletion was claimed (org_deletions state deleting)
-- and which is suspended. Every tenant-scoped table cascades from tenants. Like the operator functions it runs only
-- in a system tenant transaction and only for organizations. The checks keep a slip of the signup code from deleting
-- the wrong tenant; they are no barrier against the application role itself, which can write org_deletions and
-- suspend tenants (and already reads and writes every tenant's data).
CREATE FUNCTION rport_signup_delete_tenant(p_id text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_check(p_id);
    IF NOT EXISTS (SELECT 1 FROM org_deletions WHERE tenant_id = '_system' AND organization_id = p_id AND state = 'deleting') THEN
        RAISE EXCEPTION 'organization % has no deletion under way', p_id USING ERRCODE = 'check_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM tenants WHERE id = p_id AND status = 'suspended') THEN
        RAISE EXCEPTION 'organization % is not suspended', p_id USING ERRCODE = 'check_violation';
    END IF;
    INSERT INTO deleted_tenant_ids (id) VALUES (p_id) ON CONFLICT (id) DO NOTHING;
    DELETE FROM work_leases WHERE tenant_id = p_id;
    DELETE FROM tenants WHERE id = p_id;
    RETURN true;
END
$$;
REVOKE EXECUTE ON FUNCTION rport_signup_delete_tenant(text) FROM PUBLIC;

-- Proof of email ownership (accounts, migration 1400). email_verified_at is set when the owner of the mailbox
-- named by the login used a link mailed to it: a signup, or a signed-in account confirming its email. Accounts an
-- administrator created (invitation of a new login) start unverified; with [signup] on, an unverified account can't
-- be invited into further organizations.
ALTER TABLE accounts ADD COLUMN email_verified_at timestamptz NULL;

-- rport_account_status: whether the login has an account (exact name), whether its email is verified and whether
-- its password is expired. The application role can't see accounts of
-- other tenants' members.
CREATE FUNCTION rport_account_status(p_username text, OUT account_exists boolean, OUT verified boolean, OUT password_expired boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_app_caller();
    SELECT true, a.email_verified_at IS NOT NULL, a.password_expired INTO account_exists, verified, password_expired
        FROM accounts a WHERE a.username = p_username;
    IF NOT FOUND THEN
        account_exists := false; verified := false; password_expired := false;
    END IF;
END
$$;

CREATE FUNCTION rport_signup_check() RETURNS void LANGUAGE plpgsql STABLE AS $$
BEGIN
    PERFORM rport_app_caller();
    IF current_setting('rport.tenant_id', true) IS DISTINCT FROM '_system' THEN
        RAISE EXCEPTION 'accounts are verified only by the signup code' USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

-- rport_account_verify_email marks the email of the login verified (signup, system tenant transactions only).
CREATE FUNCTION rport_account_verify_email(p_username text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_signup_check();
    UPDATE accounts SET email_verified_at = coalesce(email_verified_at, now()) WHERE username = p_username;
    RETURN FOUND;
END
$$;

-- A renamed account names another mailbox: whoever proved the old one proved nothing about the new one.
CREATE FUNCTION rport_account_rename_unverifies() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.username IS DISTINCT FROM OLD.username THEN
        NEW.email_verified_at := NULL;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER accounts_rename_unverifies BEFORE UPDATE OF username ON accounts
    FOR EACH ROW EXECUTE FUNCTION rport_account_rename_unverifies();

-- rport_account_take_over gives the owner of the mailbox named by the login an account nobody proved to own (an
-- administrator created it), in one transaction: it joins the new organization p_org as an administrator, its
-- memberships of other organizations become pending invitations (its owner never
-- agreed to them; while they are pending, their administrators can't reset its credential), its sessions and API
-- tokens end, and it gets the password p_password, no TOTP secret, 2FA codes to the email and its email verified.
-- Returns false (nothing changed) for a verified account. Signup only (system tenant transactions), for an
-- organization it just created.
CREATE FUNCTION rport_account_take_over(p_username text, p_password text, p_org text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    t text;
BEGIN
    PERFORM rport_signup_check();
    PERFORM rport_operator_check(p_org);
    PERFORM 1 FROM accounts WHERE username = p_username AND email_verified_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    -- the other memberships become invitations
    FOR t IN SELECT tenant_id FROM user_logins WHERE username = p_username AND tenant_id <> p_org LOOP
        PERFORM set_config('rport.tenant_id', t, true);
        INSERT INTO membership_invitations (tenant_id, username, groups, invited_by)
            SELECT t, p_username, coalesce((SELECT json_agg(group_name ORDER BY group_name)::text FROM user_groups
                                            WHERE tenant_id = t AND username = p_username), '[]'), 'signup:take-over'
            ON CONFLICT (tenant_id, username) DO NOTHING;
        DELETE FROM api_sessions WHERE tenant_id = t AND username = p_username;
        DELETE FROM api_tokens WHERE tenant_id = t AND username = p_username;
    END LOOP;
    -- join the new organization first, so removing the other memberships doesn't remove the account
    PERFORM set_config('rport.tenant_id', p_org, true);
    PERFORM set_config('rport.join_account', p_username, true);
    INSERT INTO users (tenant_id, username) VALUES (p_org, p_username);
    INSERT INTO user_groups (tenant_id, username, group_name) VALUES (p_org, p_username, 'Administrators');
    DELETE FROM membership_invitations WHERE tenant_id = p_org AND username = p_username;
    PERFORM set_config('rport.join_account', '', true);
    FOR t IN SELECT tenant_id FROM user_logins WHERE username = p_username AND tenant_id <> p_org LOOP
        PERFORM set_config('rport.tenant_id', t, true);
        DELETE FROM users WHERE tenant_id = t AND username = p_username;
    END LOOP;
    UPDATE accounts SET password = p_password, password_expired = false, totp_secret = '', two_fa_send_to = p_username,
        email_verified_at = now()
        WHERE username = p_username;
    PERFORM set_config('rport.tenant_id', '_system', true);
    RETURN true;
END
$$;
REVOKE EXECUTE ON FUNCTION rport_account_status(text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_account_verify_email(text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_account_take_over(text, text, text) FROM PUBLIC;
-- the application role reads deleted_tenant_ids only (rport_signup_delete_tenant writes it); "rportd migrate
-- --grant-to" revokes the default privileges' writes on it, and so does this for roles granted before
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT DISTINCT grantee FROM information_schema.role_table_grants
             WHERE table_schema = 'public' AND table_name = 'deleted_tenant_ids' AND privilege_type = 'INSERT'
               AND grantee <> current_user LOOP
        EXECUTE format('REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON deleted_tenant_ids FROM %I', r.grantee);
    END LOOP;
END
$$;
