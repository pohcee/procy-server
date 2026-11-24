-- Devices of every tenant: the tenant a client credential belongs to, found without knowing the tenant first, and
-- the number of clients a tenant may have. Written so it can be applied again (tests re-run the backfill).

-- max_clients: the most clients the tenant may have (non-obsolete ones, connected or not). NULL: no limit. A client
-- the tenant already has is never refused; a new one is refused once the tenant has max_clients.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS max_clients integer CHECK (max_clients IS NULL OR max_clients >= 0);

-- client_auth_ids follows clients_auth (as user_logins follows users): client auth ids are unique across tenants, and
-- a connecting device names only its id, so its tenant is looked up here. No row-level security; the application
-- role may only read it (migrate.GrantApp). The credentials themselves are read in the tenant's transaction, where
-- row-level security applies.
CREATE TABLE IF NOT EXISTS client_auth_ids (
    id        text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS client_auth_ids_tenant ON client_auth_ids (tenant_id);
-- Only the trigger writes it. A database upgraded with a plain "rportd migrate" gives the new table the default
-- privileges an earlier "--grant-to" set (writes included), so they are taken back here from every role but the owner.
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT DISTINCT a.grantee::regrole AS role
             FROM pg_class c, aclexplode(c.relacl) a
             WHERE c.oid = 'client_auth_ids'::regclass AND a.grantee <> c.relowner AND a.grantee <> 0
               AND a.privilege_type IN ('INSERT', 'UPDATE', 'DELETE', 'TRUNCATE')
    LOOP
        EXECUTE format('REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON client_auth_ids FROM %s', r.role);
    END LOOP;
END
$$;

-- SECURITY DEFINER: the function (owned by the schema owner) writes client_auth_ids only for the clients_auth row
-- that fired the trigger, which row-level security already limits to the transaction's tenant.
CREATE OR REPLACE FUNCTION rport_sync_client_auth_ids() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO client_auth_ids (id, tenant_id) VALUES (NEW.id, NEW.tenant_id);
    ELSIF TG_OP = 'UPDATE' THEN
        IF NEW.id IS DISTINCT FROM OLD.id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
            DELETE FROM client_auth_ids WHERE id = OLD.id AND tenant_id = OLD.tenant_id;
            INSERT INTO client_auth_ids (id, tenant_id) VALUES (NEW.id, NEW.tenant_id);
        END IF;
    ELSE
        DELETE FROM client_auth_ids WHERE id = OLD.id AND tenant_id = OLD.tenant_id;
    END IF;
    RETURN NULL;
END
$$;
DROP TRIGGER IF EXISTS clients_auth_sync_ids ON clients_auth;
CREATE TRIGGER clients_auth_sync_ids
    AFTER INSERT OR UPDATE OF id, tenant_id OR DELETE ON clients_auth
    FOR EACH ROW EXECUTE FUNCTION rport_sync_client_auth_ids();

-- the credentials stored before this migration; row-level security is forced on clients_auth, so the owner lifts it
-- for the copy (inside this migration's transaction)
ALTER TABLE clients_auth NO FORCE ROW LEVEL SECURITY;
INSERT INTO client_auth_ids (id, tenant_id) SELECT id, tenant_id FROM clients_auth ON CONFLICT DO NOTHING;
ALTER TABLE clients_auth FORCE ROW LEVEL SECURITY;
