-- Admission of devices across the cluster. A tenant's client limit (tenants.max_clients) is decided in the database,
-- not by the process a device connects to: several processes accept devices (device processes, nodes with role
-- "all"), and each knows only the clients connected to it.

-- client_admissions: new clients admitted in the last few minutes. A client admitted by one process is counted at
-- once, before its record reaches clients or client_state (a device process ships its state through the outbox), so
-- a new client connecting to another process at that moment can't take the same place. Written only by
-- device_admit_client.
CREATE TABLE client_admissions (
    tenant_id   text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    client_id   text        NOT NULL CHECK (client_id <> ''),
    admitted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, client_id)
);
SELECT rport_enable_rls('client_admissions');
-- A database upgraded with a plain "rportd migrate" gives the new table the default privileges an earlier
-- "--grant-to" set (writes included); they are taken back from every role but the owner (as for client_auth_ids).
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT DISTINCT a.grantee::regrole AS role
             FROM pg_class c, aclexplode(c.relacl) a
             WHERE c.oid = 'client_admissions'::regclass AND a.grantee <> c.relowner AND a.grantee <> 0
               AND a.privilege_type IN ('INSERT', 'UPDATE', 'DELETE', 'TRUNCATE')
    LOOP
        EXECUTE format('REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON client_admissions FROM %s', r.role);
    END LOOP;
END
$$;

-- rport_counted_clients: the ids of the clients a tenant has, as its client limit and the operator's device count
-- count them: every client record (client_records: clients and the state device processes report) that is
-- connected or disconnected since p_cutoff (NULL: kept forever; the [server] keep_disconnected_clients rule of
-- GET /clients), every client with a live session in the registry, and the clients admitted in the last 5 minutes
-- (client_admissions). Runs with the caller's rights and row-level security: rport.tenant_id must be p_tenant.
CREATE FUNCTION rport_counted_clients(p_tenant text, p_cutoff timestamptz) RETURNS SETOF text
    LANGUAGE sql STABLE SET search_path = public, pg_temp AS $$
    SELECT r.id FROM client_records r
    WHERE r.tenant_id = p_tenant AND (r.disconnected_at IS NULL OR p_cutoff IS NULL OR r.disconnected_at >= p_cutoff)
    UNION
    SELECT s.client_id FROM client_sessions s WHERE s.tenant_id = p_tenant AND s.lease_until > now()
    UNION
    SELECT a.client_id FROM client_admissions a
    WHERE a.tenant_id = p_tenant AND a.admitted_at > now() - interval '5 minutes'
$$;

-- rport_client_counted: whether p_client_id is one of rport_counted_clients(p_tenant, p_cutoff), by key lookups
-- (primary keys of clients, client_state, client_sessions and client_admissions) instead of building the set.
CREATE FUNCTION rport_client_counted(p_tenant text, p_client_id text, p_cutoff timestamptz) RETURNS boolean
    LANGUAGE sql STABLE SET search_path = public, pg_temp AS $$
    SELECT EXISTS (SELECT 1 FROM client_records r
                   WHERE r.tenant_id = p_tenant AND r.id = p_client_id
                     AND (r.disconnected_at IS NULL OR p_cutoff IS NULL OR r.disconnected_at >= p_cutoff))
        OR EXISTS (SELECT 1 FROM client_sessions s
                   WHERE s.tenant_id = p_tenant AND s.client_id = p_client_id AND s.lease_until > now())
        OR EXISTS (SELECT 1 FROM client_admissions a
                   WHERE a.tenant_id = p_tenant AND a.client_id = p_client_id
                     AND a.admitted_at > now() - interval '5 minutes')
$$;

-- device_admit_client decides whether tenant p_tenant takes the device p_client_id now connecting (p_cutoff as in
-- rport_counted_clients). decision is
--   'admitted'        the tenant is active and either has no limit, already has the client (it is counted: an
--                     existing device is never refused, even with the tenant over its limit) or has a place left,
--                     which the client now takes;
--   'refused_limit'   a new client of a tenant that has max_clients clients;
--   'refused_status'  the tenant is not active (tenant_status says what it is, 'deleted' when it doesn't exist).
-- clients is the tenant's count (rport_counted_clients) when a new client was counted: the new client included when
-- admitted; 0 when the tenant has no limit or the client is one it has. A client the tenant has is found by key
-- lookups without the lock, so a reconnect storm of existing devices neither waits on the lock nor counts the
-- tenant. New clients of a limited tenant are admitted one at a time across the cluster (an advisory lock per tenant
-- for the transaction), so two processes can't both take the last place. The lock serializes only under READ
-- COMMITTED (each statement after it sees what the previous holder committed), so other isolation levels are refused.
CREATE FUNCTION device_admit_client(p_tenant text, p_client_id text, p_cutoff timestamptz)
    RETURNS TABLE (decision text, tenant_status text, clients integer, max_clients integer)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    prev_tenant text := current_setting('rport.tenant_id', true);
    st          text;
    mx          integer;
    n           integer := 0;
BEGIN
    IF p_tenant IS NULL OR p_client_id IS NULL OR p_client_id = '' OR length(p_client_id) > 1000 THEN
        RAISE EXCEPTION 'invalid tenant or client id' USING ERRCODE = '22023';
    END IF;
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'device_admit_client needs READ COMMITTED, not %', current_setting('transaction_isolation')
            USING ERRCODE = '25001';
    END IF;
    SELECT t.status, t.max_clients INTO st, mx FROM tenants t WHERE t.id = p_tenant;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'refused_status'::text, 'deleted'::text, 0, NULL::integer;
        RETURN;
    END IF;
    IF st <> 'active' THEN
        RETURN QUERY SELECT 'refused_status'::text, st, 0, mx;
        RETURN;
    END IF;
    IF mx IS NULL THEN
        RETURN QUERY SELECT 'admitted'::text, st, 0, mx;
        RETURN;
    END IF;
    PERFORM set_config('rport.tenant_id', p_tenant, true);
    decision := 'admitted';
    IF NOT rport_client_counted(p_tenant, p_client_id, p_cutoff) THEN
        PERFORM pg_advisory_xact_lock(hashtextextended('rport/device-admission/' || p_tenant, 0));
        -- again under the lock: another connection of the same client may have taken its place meanwhile
        IF NOT rport_client_counted(p_tenant, p_client_id, p_cutoff) THEN
            DELETE FROM client_admissions a WHERE a.tenant_id = p_tenant AND a.admitted_at <= now() - interval '5 minutes';
            SELECT count(*) INTO n FROM rport_counted_clients(p_tenant, p_cutoff);
            IF n >= mx THEN
                decision := 'refused_limit';
            ELSE
                INSERT INTO client_admissions (tenant_id, client_id) VALUES (p_tenant, p_client_id)
                ON CONFLICT ON CONSTRAINT client_admissions_pkey DO UPDATE SET admitted_at = now();
                n := n + 1;
            END IF;
        END IF;
    END IF;
    PERFORM set_config('rport.tenant_id', coalesce(prev_tenant, ''), true);
    RETURN QUERY SELECT decision, st, n, mx;
END
$$;

-- Not executable by PUBLIC; granted to the application role (migrate.GrantApp, nodes with role "all") and the device
-- role (migrate.GrantDevice). Roles granted before this migration are recognized by their EXECUTE on the session
-- registry (migration 1200), which both have, so a plain "rportd migrate" keeps their device admission working.
REVOKE EXECUTE ON FUNCTION device_admit_client(text, text, timestamptz) FROM PUBLIC;
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT DISTINCT a.grantee::regrole AS role
             FROM pg_proc p, aclexplode(p.proacl) a
             WHERE p.oid = 'rport_register_session(text, text, text, text, integer)'::regprocedure
               AND a.grantee <> p.proowner AND a.grantee <> 0 AND a.privilege_type = 'EXECUTE'
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION device_admit_client(text, text, timestamptz) TO %s', r.role);
    END LOOP;
END
$$;
