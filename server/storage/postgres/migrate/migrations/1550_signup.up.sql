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
    -- pending: link sent; existing: the email is already a user (a notice was sent, no link); awaiting_approval:
    -- verified, held for an operator; creating: an operator's approval is creating the organization; completed;
    -- denied; expired
    status          text        NOT NULL CHECK (status IN ('pending', 'existing', 'awaiting_approval', 'creating', 'completed', 'denied', 'expired')),
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
