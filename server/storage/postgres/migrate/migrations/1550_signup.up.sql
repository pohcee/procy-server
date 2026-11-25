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
    -- verified, held for an operator; completed; denied; expired
    status          text        NOT NULL CHECK (status IN ('pending', 'existing', 'awaiting_approval', 'completed', 'denied', 'expired')),
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
    state             text        NOT NULL CHECK (state IN ('pending', 'canceled', 'done')),
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
CREATE UNIQUE INDEX org_deletions_one_pending ON org_deletions (organization_id) WHERE state = 'pending';
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

-- rport_signup_delete_tenant deletes an organization whose deletion is due: a pending org_deletions row with
-- delete_after in the past and the tenant suspended. Every tenant-scoped table cascades from tenants. Like the
-- operator functions it runs only in a system tenant transaction and only for organizations. Returns false (and
-- deletes nothing) when the organization is not suspended (an operator reactivated it).
CREATE FUNCTION rport_signup_delete_tenant(p_id text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_check(p_id);
    IF NOT EXISTS (SELECT 1 FROM org_deletions WHERE tenant_id = '_system' AND organization_id = p_id
                   AND state = 'pending' AND delete_after <= now()) THEN
        RAISE EXCEPTION 'organization % has no deletion due', p_id USING ERRCODE = 'check_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM tenants WHERE id = p_id AND status = 'suspended') THEN
        RETURN false;
    END IF;
    INSERT INTO deleted_tenant_ids (id) VALUES (p_id) ON CONFLICT (id) DO NOTHING;
    DELETE FROM work_leases WHERE tenant_id = p_id;
    DELETE FROM tenants WHERE id = p_id;
    RETURN true;
END
$$;
REVOKE EXECUTE ON FUNCTION rport_signup_delete_tenant(text) FROM PUBLIC;
