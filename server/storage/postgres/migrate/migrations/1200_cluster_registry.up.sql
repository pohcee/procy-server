-- The cluster registry: the device processes of the cluster (nodes) and which one holds each client's connection
-- (client_sessions). Device processes write both only through the SECURITY DEFINER functions below, which check their
-- input; api processes read the tables to find the process to pass work on a client to.

-- nodes: one row per node_id, written by its device process's heartbeat. Not tenant-scoped.
CREATE TABLE nodes (
    node_id      text PRIMARY KEY CHECK (node_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$'),
    region       text        NOT NULL DEFAULT '' CHECK (length(region) <= 63),
    -- empty when the process answers no peer calls
    peer_address text        NOT NULL DEFAULT '' CHECK (length(peer_address) <= 255),
    heartbeat_at timestamptz NOT NULL DEFAULT now()
);

-- client_sessions: one row per connected client. A client connecting to a second node replaces the row (the newest
-- session wins); the node holding the older session closes it when renewing tells it the session was taken over.
-- Rows whose lease ran out are ignored and removed later.
CREATE TABLE client_sessions (
    tenant_id    text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    client_id    text        NOT NULL,
    node_id      text        NOT NULL,
    session_id   text        NOT NULL UNIQUE,
    connected_at timestamptz NOT NULL DEFAULT now(),
    lease_until  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, client_id)
);
CREATE INDEX client_sessions_node ON client_sessions (node_id);
CREATE INDEX client_sessions_lease ON client_sessions (lease_until);
-- Row-level security by tenant for readers; not forced, so the functions below (running as the table owner) write
-- the sessions of every tenant.
ALTER TABLE client_sessions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON client_sessions
    USING (tenant_id = current_setting('rport.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('rport.tenant_id', true));

CREATE FUNCTION rport_check_registry_args(p_node text, p_lease_seconds integer) RETURNS void
    LANGUAGE plpgsql IMMUTABLE SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF p_node IS NULL OR p_node !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$' THEN
        RAISE EXCEPTION 'invalid node id %', p_node USING ERRCODE = '22023';
    END IF;
    IF p_lease_seconds IS NULL OR p_lease_seconds < 1 OR p_lease_seconds > 3600 THEN
        RAISE EXCEPTION 'invalid lease % s (1 to 3600)', p_lease_seconds USING ERRCODE = '22023';
    END IF;
END
$$;

-- rport_node_heartbeat records that the device process of p_node is up and reachable at p_peer_address.
CREATE FUNCTION rport_node_heartbeat(p_node text, p_region text, p_peer_address text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    PERFORM rport_check_registry_args(p_node, 1);
    INSERT INTO nodes (node_id, region, peer_address, heartbeat_at)
    VALUES (p_node, coalesce(p_region, ''), coalesce(p_peer_address, ''), now())
    ON CONFLICT (node_id) DO UPDATE
        SET region = EXCLUDED.region, peer_address = EXCLUDED.peer_address, heartbeat_at = EXCLUDED.heartbeat_at;
END
$$;

CREATE FUNCTION rport_check_session_args(p_tenant text, p_client text, p_session text) RETURNS void
    LANGUAGE plpgsql STABLE SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF p_client IS NULL OR p_client = '' OR length(p_client) > 255 THEN
        RAISE EXCEPTION 'invalid client id' USING ERRCODE = '22023';
    END IF;
    IF p_session IS NULL OR p_session !~ '^[A-Za-z0-9_-]{8,64}$' THEN
        RAISE EXCEPTION 'invalid session id' USING ERRCODE = '22023';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM tenants WHERE id = p_tenant AND status = 'active') THEN
        RAISE EXCEPTION 'tenant % is not active', p_tenant USING ERRCODE = '22023';
    END IF;
END
$$;

-- rport_register_session records a new connection of a client to p_node. It replaces any other session of the
-- client: the newest session wins.
CREATE FUNCTION rport_register_session(p_tenant text, p_client text, p_node text, p_session text, p_lease_seconds integer)
    RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    PERFORM rport_check_registry_args(p_node, p_lease_seconds);
    PERFORM rport_check_session_args(p_tenant, p_client, p_session);
    INSERT INTO client_sessions (tenant_id, client_id, node_id, session_id, connected_at, lease_until)
    VALUES (p_tenant, p_client, p_node, p_session, now(), now() + make_interval(secs => p_lease_seconds))
    ON CONFLICT (tenant_id, client_id) DO UPDATE
        SET node_id = EXCLUDED.node_id, session_id = EXCLUDED.session_id,
            connected_at = EXCLUDED.connected_at, lease_until = EXCLUDED.lease_until;
END
$$;

-- rport_renew_sessions extends the leases of the sessions p_node holds (p_sessions: a JSON array of
-- {"tenant_id", "client_id", "session_id"}). A session whose row is gone (it expired and was removed) is recorded
-- again. A held client stored as disconnected (the sweep ran while the lease had lapsed) is stored connected again.
-- It returns the ids of the sessions p_node no longer holds (another session replaced them, or their tenant is gone
-- or not active): p_node closes those connections.
CREATE FUNCTION rport_renew_sessions(p_node text, p_sessions jsonb, p_lease_seconds integer)
    RETURNS SETOF text LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    s record;
    cur text;
BEGIN
    PERFORM rport_check_registry_args(p_node, p_lease_seconds);
    IF jsonb_typeof(p_sessions) IS DISTINCT FROM 'array' THEN
        RAISE EXCEPTION 'sessions must be a JSON array' USING ERRCODE = '22023';
    END IF;
    FOR s IN SELECT * FROM jsonb_to_recordset(p_sessions) AS x(tenant_id text, client_id text, session_id text) LOOP
        BEGIN
            SELECT session_id INTO cur FROM client_sessions
                WHERE tenant_id = s.tenant_id AND client_id = s.client_id FOR UPDATE;
            IF NOT FOUND THEN
                -- recorded again unless a new session of the client (or another row with this id) got there first
                PERFORM rport_check_session_args(s.tenant_id, s.client_id, s.session_id);
                INSERT INTO client_sessions (tenant_id, client_id, node_id, session_id, connected_at, lease_until)
                VALUES (s.tenant_id, s.client_id, p_node, s.session_id, now(), now() + make_interval(secs => p_lease_seconds))
                ON CONFLICT DO NOTHING;
                IF NOT FOUND THEN
                    RETURN NEXT s.session_id;
                    CONTINUE;
                END IF;
            ELSIF cur = s.session_id THEN
                UPDATE client_sessions SET node_id = p_node, lease_until = now() + make_interval(secs => p_lease_seconds)
                    WHERE tenant_id = s.tenant_id AND client_id = s.client_id;
            ELSE
                RETURN NEXT s.session_id;
                CONTINUE;
            END IF;
            -- the client is connected: undo a sweep that marked it disconnected while the lease had run out
            PERFORM set_config('rport.tenant_id', s.tenant_id, true);
            UPDATE clients SET disconnected_at = NULL
                WHERE tenant_id = s.tenant_id AND id = s.client_id AND disconnected_at IS NOT NULL;
        EXCEPTION WHEN invalid_parameter_value OR foreign_key_violation THEN
            -- the session can't be held any more (its tenant was deleted or suspended): this one is lost, not the batch
            RETURN NEXT s.session_id;
        END;
    END LOOP;
    -- expired long ago: nobody renews them any more
    DELETE FROM client_sessions WHERE lease_until < now() - interval '1 hour';
END
$$;

-- rport_end_session removes a session when its connection ends; a newer session of the client stays.
CREATE FUNCTION rport_end_session(p_session text) RETURNS void
    LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
    DELETE FROM client_sessions WHERE session_id = p_session;
$$;

REVOKE ALL ON FUNCTION rport_check_registry_args(text, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION rport_check_session_args(text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION rport_node_heartbeat(text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION rport_register_session(text, text, text, text, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION rport_renew_sessions(text, jsonb, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION rport_end_session(text) FROM PUBLIC;
