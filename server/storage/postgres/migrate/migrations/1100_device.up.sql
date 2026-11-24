-- Device processes (nodes with [storage] role = "device"). They connect as the device database role, which has no
-- table privileges at all: it reads the device path through the device_* functions and writes what devices report
-- through the ingest_* functions and report_client_state, which check their input (migrate.GrantDevice grants
-- EXECUTE on exactly these). Every function is SECURITY DEFINER (runs as the schema owner) and pins search_path.

-- What a device reports about itself (system info, addresses, version, update status, connection state), kept apart
-- from what admins set in clients: the device role can write only this table, and only through
-- report_client_state. A device's first report creates its row; a clients row exists only once something else
-- (an admin, or a node with role "all") saved the client. details is the stored client document (as in clients)
-- without the admin-set keys.
CREATE TABLE client_state (
    tenant_id       text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    client_id       text        NOT NULL CHECK (client_id <> ''),
    client_auth_id  text        NOT NULL,
    disconnected_at timestamptz NULL,
    details         jsonb       NOT NULL CHECK (jsonb_typeof(details) = 'object'),
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    reported_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, client_id)
);
SELECT rport_enable_rls('client_state');
-- api processes refresh a client when its reported state changes, as when its record does (change feed, 0120)
SELECT rport_track_changes('client_state', 'clients', 'client_id');

-- Connections and disconnections of clients as device processes saw them.
CREATE TABLE client_connection_events (
    tenant_id      text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id             bigserial   NOT NULL,
    client_id      text        NOT NULL,
    client_auth_id text        NOT NULL,
    event          text        NOT NULL CHECK (event IN ('connected', 'disconnected')),
    at             timestamptz NOT NULL,
    node_id        text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX client_connection_events_client ON client_connection_events (tenant_id, client_id, at DESC);
SELECT rport_enable_rls('client_connection_events');

-- Idempotency keys of the ingested items (measurements use their primary key instead): an item whose key is here
-- was applied already and is reported as a duplicate. Pruned by the cluster upkeep of the api processes.
CREATE TABLE ingest_keys (
    tenant_id   text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    kind        text        NOT NULL,
    key         text        NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, kind, key)
);
CREATE INDEX ingest_keys_received ON ingest_keys (received_at);
SELECT rport_enable_rls('ingest_keys');

-- rport_admin_groups returns the allowed_user_groups array of a stored client document ('[]' when there is none or
-- the document can't be read as jsonb).
CREATE FUNCTION rport_admin_groups(doc text) RETURNS jsonb LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE v jsonb;
BEGIN
    IF doc IS NULL THEN
        RETURN '[]'::jsonb;
    END IF;
    BEGIN
        v := doc::jsonb -> 'allowed_user_groups';
    EXCEPTION WHEN others THEN
        v := NULL;
    END;
    IF v IS NULL OR jsonb_typeof(v) <> 'array' THEN
        RETURN '[]'::jsonb;
    END IF;
    RETURN v;
END
$$;

-- client_records is what the API reads as the clients: the admin-set record joined with the device-reported state.
-- With a state row, everything comes from it except allowed_user_groups, which only an admin sets (taken from
-- clients, empty without a clients row). Without one, the clients row as it is (nodes with role "all" keep
-- everything there). A UNION ALL rather than a FULL JOIN, so a filter on tenant_id and id reaches the primary keys.
-- security_invoker: row-level security of the reader applies.
CREATE VIEW client_records WITH (security_invoker = true) AS
SELECT c.tenant_id,
       c.id,
       CASE WHEN s.client_id IS NULL THEN c.client_auth_id ELSE s.client_auth_id END   AS client_auth_id,
       CASE WHEN s.client_id IS NULL THEN c.disconnected_at ELSE s.disconnected_at END AS disconnected_at,
       CASE WHEN s.client_id IS NULL THEN c.details
            ELSE ((s.details - 'allowed_user_groups')
                  || jsonb_build_object('allowed_user_groups', rport_admin_groups(c.details)))::text
       END AS details
FROM clients c
LEFT JOIN client_state s ON s.tenant_id = c.tenant_id AND s.client_id = c.id
UNION ALL
SELECT s.tenant_id,
       s.client_id,
       s.client_auth_id,
       s.disconnected_at,
       ((s.details - 'allowed_user_groups') || '{"allowed_user_groups": []}'::jsonb)::text
FROM client_state s
WHERE NOT EXISTS (SELECT 1 FROM clients c WHERE c.tenant_id = s.tenant_id AND c.id = s.client_id);

-- Cross-tenant reads for the device_* functions: while the transaction-local setting rport.cross_tenant_read is
-- 'on', the role that ran this migration (the schema owner, which the SECURITY DEFINER functions run as) reads these
-- tables' rows of every tenant. Other roles are not named in the policy, so the setting does nothing for them.
DO $$
DECLARE tbl text;
BEGIN
    FOREACH tbl IN ARRAY ARRAY['clients_auth', 'clients', 'client_state', 'client_groups', 'stored_tunnels'] LOOP
        EXECUTE format('CREATE POLICY cross_tenant_read ON %I FOR SELECT TO CURRENT_USER '
                       'USING (current_setting(''rport.cross_tenant_read'', true) = ''on'')', tbl);
    END LOOP;
END
$$;

-- ---------------------------------------------------------------------------------------------------------------
-- Device-path lookups. The device role gets lookups by key only, no snapshot of whole tables and no change feed.

-- device_tenants: the tenants and their status (a device process admits devices of active tenants only).
CREATE FUNCTION device_tenants() RETURNS TABLE (id text, status text)
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT t.id, t.status FROM tenants t ORDER BY t.id
$$;

-- device_client_auth: the tenant and argon2id hash of a client credential, looked up by its id (unique across
-- tenants), for credentials of active tenants. Credentials not stored as a hash (written before hashing, see
-- rportd migrate) are not returned: the device role never sees a password.
CREATE FUNCTION device_client_auth(p_id text) RETURNS TABLE (tenant_id text, id text, password_hash text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY SELECT a.tenant_id, a.id, a.password
    FROM clients_auth a JOIN tenants t ON t.id = a.tenant_id
    WHERE a.id = p_id AND t.status = 'active' AND a.password LIKE '$argon2id$%';
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- device_client: one client record of a tenant as client_records shows it.
CREATE FUNCTION device_client(p_tenant text, p_id text) RETURNS TABLE (id text, client_auth_id text, disconnected_at timestamptz, details text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY SELECT r.id, r.client_auth_id, r.disconnected_at, r.details FROM client_records r
    WHERE r.tenant_id = p_tenant AND r.id = p_id;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- device_client_groups: a tenant's client groups (all of them: a client's groups are found by matching every group).
CREATE FUNCTION device_client_groups(p_tenant text) RETURNS TABLE (id text, description text, params text, allowed_user_groups text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY SELECT g.id, g.description, g.params, g.allowed_user_groups FROM client_groups g
    WHERE g.tenant_id = p_tenant ORDER BY g.id;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- device_stored_tunnels: the stored tunnels of a tenant's client.
CREATE FUNCTION device_stored_tunnels(p_tenant text, p_client_id text) RETURNS TABLE (id text, client_id text, created_at timestamptz, name text, scheme text, remote_ip text,
                   remote_port integer, acl text, public_port integer, further_options text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY SELECT s.id, s.client_id, s.created_at, s.name, s.scheme, s.remote_ip, s.remote_port, s.acl, s.public_port,
           s.further_options
    FROM stored_tunnels s WHERE s.tenant_id = p_tenant AND s.client_id = p_client_id ORDER BY s.id;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- ---------------------------------------------------------------------------------------------------------------
-- Ingest. Each function takes a jsonb array of at most 1000 items; every item has "tenant_id" and "key" (its
-- idempotency key, 1–200 characters) and the fields of its kind. It returns one row per item: out_status is
-- 'applied', 'duplicate' (the key, or for measurements the client and time, was applied before) or 'rejected: <why>'
-- (invalid item; nothing written). A batch that is not such an array raises an error. Each item is written with
-- rport.tenant_id set to its tenant, so row-level security checks the tenant as for the application role.

-- rport_ingest_batch checks the shape of a batch.
CREATE FUNCTION rport_ingest_batch(batch jsonb) RETURNS void LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    IF batch IS NULL OR jsonb_typeof(batch) <> 'array' THEN
        RAISE EXCEPTION 'ingest: batch must be a JSON array' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF jsonb_array_length(batch) > 1000 THEN
        RAISE EXCEPTION 'ingest: batch has % items, at most 1000 are accepted', jsonb_array_length(batch)
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
END
$$;

-- rport_ingest_item checks the common fields of an item and, when they are valid, sets the transaction's tenant to
-- the item's. It returns why the item is rejected, or NULL.
CREATE FUNCTION rport_ingest_item(it jsonb) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE t text := it ->> 'tenant_id'; k text := it ->> 'key';
BEGIN
    IF jsonb_typeof(it) IS DISTINCT FROM 'object' THEN
        RETURN 'item is not an object';
    END IF;
    IF k IS NULL OR length(k) < 1 OR length(k) > 200 THEN
        RETURN 'key must have 1 to 200 characters';
    END IF;
    IF t IS NULL OR NOT EXISTS (SELECT 1 FROM tenants WHERE id = t AND status = 'active') THEN
        RETURN 'unknown or inactive tenant';
    END IF;
    PERFORM set_config('rport.tenant_id', t, true);
    RETURN NULL;
END
$$;

-- rport_ingest_text returns the text field f of it, or raises (caught per item) when it is missing, empty (unless
-- empty_ok) or longer than max_len.
CREATE FUNCTION rport_ingest_text(it jsonb, f text, max_len integer, empty_ok boolean DEFAULT false) RETURNS text
    LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE v text := it ->> f;
BEGIN
    IF v IS NULL OR jsonb_typeof(it -> f) <> 'string' THEN
        RAISE EXCEPTION '% must be a string', f USING ERRCODE = 'data_exception';
    END IF;
    IF (NOT empty_ok AND v = '') OR length(v) > max_len THEN
        RAISE EXCEPTION '% must have % to % characters', f, CASE WHEN empty_ok THEN 0 ELSE 1 END, max_len
            USING ERRCODE = 'data_exception';
    END IF;
    RETURN v;
END
$$;

-- rport_ingest_claim records the idempotency key of an item; false when it was recorded before.
CREATE FUNCTION rport_ingest_claim(it jsonb, kind text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    INSERT INTO ingest_keys (tenant_id, kind, key) VALUES (it ->> 'tenant_id', kind, it ->> 'key') ON CONFLICT DO NOTHING;
    RETURN FOUND;
END
$$;

-- rport_job_library_merge returns the job document incoming with the library fields (library_entry_id,
-- library_version, os) of stored where incoming has none (older clients don't echo them back). incoming as it is
-- when nothing is added or either document can't be read as jsonb.
CREATE FUNCTION rport_job_library_merge(stored text, incoming text) RETURNS text LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE s jsonb; i jsonb; o jsonb; k text;
BEGIN
    IF stored IS NULL OR incoming IS NULL THEN
        RETURN incoming;
    END IF;
    BEGIN
        s := stored::jsonb;
        i := incoming::jsonb;
    EXCEPTION WHEN others THEN
        RETURN incoming;
    END;
    IF jsonb_typeof(s) <> 'object' OR jsonb_typeof(i) <> 'object' THEN
        RETURN incoming;
    END IF;
    o := i;
    FOREACH k IN ARRAY ARRAY['library_entry_id', 'library_version', 'os'] LOOP
        IF (o -> k) IS NULL AND (s -> k) IS NOT NULL THEN
            o := o || jsonb_build_object(k, s -> k);
        END IF;
    END LOOP;
    IF o = i THEN
        RETURN incoming;
    END IF;
    RETURN o::text;
END
$$;

-- ingest_job_results: job results. Fields: jid, client_id, status, started_at, finished_at (null while running),
-- created_by, multi_job_id (null or an existing multi-job), details (the job document as text, valid JSON), and
-- create_only (optional). Creates the job or replaces it, as the job store's SaveJob, keeping the stored library
-- fields the item lacks; with create_only = true, as CreateJob: an existing job is kept, only the item's library
-- fields are added to it (a fast result can arrive before the job's creation). A job of another client is rejected.
CREATE FUNCTION ingest_job_results(batch jsonb) RETURNS TABLE (out_key text, out_status text)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev_tenant text := current_setting('rport.tenant_id', true); it jsonb; why text; v_jid text; v_client text; v_multi text; v_owner text; v_details text; v_create boolean;
BEGIN
    PERFORM rport_ingest_batch(batch);
    FOR it IN SELECT e FROM jsonb_array_elements(batch) AS e LOOP
        out_key := coalesce(it ->> 'key', '');
        out_status := 'applied';
        why := rport_ingest_item(it);
        IF why IS NULL THEN
            BEGIN
                v_jid := rport_ingest_text(it, 'jid', 200);
                v_client := rport_ingest_text(it, 'client_id', 200);
                v_multi := nullif(it ->> 'multi_job_id', '');
                v_details := rport_ingest_text(it, 'details', 16777216);
                PERFORM v_details::json;
                v_create := coalesce((it ->> 'create_only')::boolean, false);
                IF (it ->> 'status') NOT IN ('running', 'successful', 'failed', 'unknown', 'skipped') THEN
                    RAISE EXCEPTION 'invalid status %', it ->> 'status' USING ERRCODE = 'data_exception';
                END IF;
                IF v_multi IS NOT NULL AND NOT EXISTS (
                        SELECT 1 FROM multi_jobs m WHERE m.tenant_id = it ->> 'tenant_id' AND m.jid = v_multi) THEN
                    RAISE EXCEPTION 'unknown multi-job' USING ERRCODE = 'data_exception';
                END IF;
                SELECT j.client_id INTO v_owner FROM jobs j WHERE j.tenant_id = it ->> 'tenant_id' AND j.jid = v_jid;
                IF FOUND AND v_owner <> v_client THEN
                    RAISE EXCEPTION 'job belongs to another client' USING ERRCODE = 'data_exception';
                END IF;
                IF NOT rport_ingest_claim(it, 'job_result') THEN
                    out_status := 'duplicate';
                ELSIF v_create THEN
                    INSERT INTO jobs (tenant_id, jid, status, started_at, finished_at, created_by, client_id,
                                      multi_job_id, details)
                    VALUES (it ->> 'tenant_id', v_jid, it ->> 'status', (it ->> 'started_at')::timestamptz,
                            (it ->> 'finished_at')::timestamptz, rport_ingest_text(it, 'created_by', 1000, true),
                            v_client, v_multi, v_details)
                    ON CONFLICT (tenant_id, jid) DO UPDATE SET
                        details = rport_job_library_merge(EXCLUDED.details, jobs.details);
                ELSE
                    INSERT INTO jobs (tenant_id, jid, status, started_at, finished_at, created_by, client_id,
                                      multi_job_id, details)
                    VALUES (it ->> 'tenant_id', v_jid, it ->> 'status', (it ->> 'started_at')::timestamptz,
                            (it ->> 'finished_at')::timestamptz, rport_ingest_text(it, 'created_by', 1000, true),
                            v_client, v_multi, v_details)
                    ON CONFLICT (tenant_id, jid) DO UPDATE SET
                        status = EXCLUDED.status, started_at = EXCLUDED.started_at,
                        finished_at = EXCLUDED.finished_at, created_by = EXCLUDED.created_by,
                        multi_job_id = EXCLUDED.multi_job_id,
                        details = rport_job_library_merge(jobs.details, EXCLUDED.details);
                END IF;
            EXCEPTION WHEN data_exception OR integrity_constraint_violation OR program_limit_exceeded THEN
                why := SQLERRM;
            END;
        END IF;
        IF why IS NOT NULL THEN
            out_status := 'rejected: ' || why;
        END IF;
        RETURN NEXT;
    END LOOP;
    PERFORM set_config('rport.tenant_id', coalesce(prev_tenant, ''), true);
END
$$;

-- ingest_measurements: monitoring measurements. Fields: client_id, timestamp, cpu_usage_percent,
-- memory_usage_percent, io_usage_percent (numbers), processes, mountpoints (null, empty or JSON text), net_lan_in,
-- net_lan_out, net_wan_in, net_wan_out (null or integers). A client has one measurement per time: a second one is a
-- duplicate.
CREATE FUNCTION ingest_measurements(batch jsonb) RETURNS TABLE (out_key text, out_status text)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev_tenant text := current_setting('rport.tenant_id', true); it jsonb; why text;
BEGIN
    PERFORM rport_ingest_batch(batch);
    FOR it IN SELECT e FROM jsonb_array_elements(batch) AS e LOOP
        out_key := coalesce(it ->> 'key', '');
        out_status := 'applied';
        why := rport_ingest_item(it);
        IF why IS NULL THEN
            BEGIN
                IF coalesce(it ->> 'processes', '') <> '' THEN PERFORM (it ->> 'processes')::json; END IF;
                IF coalesce(it ->> 'mountpoints', '') <> '' THEN PERFORM (it ->> 'mountpoints')::json; END IF;
                INSERT INTO measurements (tenant_id, client_id, timestamp, cpu_usage_percent, memory_usage_percent,
                                          io_usage_percent, processes, mountpoints, net_lan_in, net_lan_out,
                                          net_wan_in, net_wan_out)
                VALUES (it ->> 'tenant_id', rport_ingest_text(it, 'client_id', 200), (it ->> 'timestamp')::timestamptz,
                        (it -> 'cpu_usage_percent')::double precision, (it -> 'memory_usage_percent')::double precision,
                        (it -> 'io_usage_percent')::double precision, it ->> 'processes', it ->> 'mountpoints',
                        (it ->> 'net_lan_in')::bigint, (it ->> 'net_lan_out')::bigint,
                        (it ->> 'net_wan_in')::bigint, (it ->> 'net_wan_out')::bigint)
                ON CONFLICT DO NOTHING;
                IF NOT FOUND THEN
                    out_status := 'duplicate';
                END IF;
            EXCEPTION WHEN data_exception OR integrity_constraint_violation OR program_limit_exceeded THEN
                why := SQLERRM;
            END;
        END IF;
        IF why IS NOT NULL THEN
            out_status := 'rejected: ' || why;
        END IF;
        RETURN NEXT;
    END LOOP;
    PERFORM set_config('rport.tenant_id', coalesce(prev_tenant, ''), true);
END
$$;

-- ingest_connection_events: fields client_id, client_auth_id, event ('connected' or 'disconnected'), at, node_id
-- (optional).
CREATE FUNCTION ingest_connection_events(batch jsonb) RETURNS TABLE (out_key text, out_status text)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev_tenant text := current_setting('rport.tenant_id', true); it jsonb; why text;
BEGIN
    PERFORM rport_ingest_batch(batch);
    FOR it IN SELECT e FROM jsonb_array_elements(batch) AS e LOOP
        out_key := coalesce(it ->> 'key', '');
        out_status := 'applied';
        why := rport_ingest_item(it);
        IF why IS NULL THEN
            BEGIN
                IF NOT rport_ingest_claim(it, 'connection_event') THEN
                    out_status := 'duplicate';
                ELSE
                    INSERT INTO client_connection_events (tenant_id, client_id, client_auth_id, event, at, node_id)
                    VALUES (it ->> 'tenant_id', rport_ingest_text(it, 'client_id', 200),
                            rport_ingest_text(it, 'client_auth_id', 200), it ->> 'event',
                            (it ->> 'at')::timestamptz, CASE WHEN it ? 'node_id' THEN rport_ingest_text(it, 'node_id', 200, true) ELSE '' END);
                END IF;
            EXCEPTION WHEN data_exception OR integrity_constraint_violation OR program_limit_exceeded THEN
                why := SQLERRM;
            END;
        END IF;
        IF why IS NOT NULL THEN
            out_status := 'rejected: ' || why;
        END IF;
        RETURN NEXT;
    END LOOP;
    PERFORM set_config('rport.tenant_id', coalesce(prev_tenant, ''), true);
END
$$;

-- ingest_audit: audit log entries. Fields: timestamp, application, action (required), username, remote_ip,
-- affected_id, client_id, client_hostname, request, response (strings, may be empty).
CREATE FUNCTION ingest_audit(batch jsonb) RETURNS TABLE (out_key text, out_status text)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev_tenant text := current_setting('rport.tenant_id', true); it jsonb; why text;
BEGIN
    PERFORM rport_ingest_batch(batch);
    FOR it IN SELECT e FROM jsonb_array_elements(batch) AS e LOOP
        out_key := coalesce(it ->> 'key', '');
        out_status := 'applied';
        why := rport_ingest_item(it);
        IF why IS NULL THEN
            BEGIN
                IF NOT rport_ingest_claim(it, 'audit') THEN
                    out_status := 'duplicate';
                ELSE
                    INSERT INTO auditlog (tenant_id, timestamp, application, action, username, remote_ip,
                                          affected_id, client_id, client_hostname, request, response)
                    VALUES (it ->> 'tenant_id', (it ->> 'timestamp')::timestamptz,
                            rport_ingest_text(it, 'application', 200), rport_ingest_text(it, 'action', 200),
                            it ->> 'username', it ->> 'remote_ip', it ->> 'affected_id', it ->> 'client_id',
                            it ->> 'client_hostname', it ->> 'request', it ->> 'response');
                END IF;
            EXCEPTION WHEN data_exception OR integrity_constraint_violation OR program_limit_exceeded THEN
                why := SQLERRM;
            END;
        END IF;
        IF why IS NOT NULL THEN
            out_status := 'rejected: ' || why;
        END IF;
        RETURN NEXT;
    END LOOP;
    PERFORM set_config('rport.tenant_id', coalesce(prev_tenant, ''), true);
END
$$;

-- report_client_state: device-reported client state. Fields: client_id, client_auth_id (a credential of the
-- item's tenant), disconnected_at (null while connected), reported_at, details (the stored client document, a JSON
-- object; allowed_user_groups is dropped, only admins set it). A report older than the stored one is a duplicate.
CREATE FUNCTION report_client_state(batch jsonb) RETURNS TABLE (out_key text, out_status text)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev_tenant text := current_setting('rport.tenant_id', true); it jsonb; why text; v_client text; v_auth text; v_details jsonb; v_at timestamptz;
BEGIN
    PERFORM rport_ingest_batch(batch);
    FOR it IN SELECT e FROM jsonb_array_elements(batch) AS e LOOP
        out_key := coalesce(it ->> 'key', '');
        out_status := 'applied';
        why := rport_ingest_item(it);
        IF why IS NULL THEN
            BEGIN
                v_client := rport_ingest_text(it, 'client_id', 200);
                v_auth := rport_ingest_text(it, 'client_auth_id', 200);
                v_at := (it ->> 'reported_at')::timestamptz;
                IF v_at IS NULL THEN
                    RAISE EXCEPTION 'reported_at is required' USING ERRCODE = 'data_exception';
                END IF;
                v_details := rport_ingest_text(it, 'details', 1048576)::jsonb;
                IF jsonb_typeof(v_details) <> 'object' THEN
                    RAISE EXCEPTION 'details must be a JSON object' USING ERRCODE = 'data_exception';
                END IF;
                IF NOT EXISTS (SELECT 1 FROM clients_auth a WHERE a.tenant_id = it ->> 'tenant_id' AND a.id = v_auth) THEN
                    RAISE EXCEPTION 'unknown client credential' USING ERRCODE = 'data_exception';
                END IF;
                IF NOT rport_ingest_claim(it, 'client_state') THEN
                    out_status := 'duplicate';
                ELSE
                    INSERT INTO client_state (tenant_id, client_id, client_auth_id, disconnected_at, details, reported_at)
                    VALUES (it ->> 'tenant_id', v_client, v_auth, (it ->> 'disconnected_at')::timestamptz,
                            v_details - 'allowed_user_groups', v_at)
                    ON CONFLICT (tenant_id, client_id) DO UPDATE SET
                        client_auth_id = EXCLUDED.client_auth_id, disconnected_at = EXCLUDED.disconnected_at,
                        details = EXCLUDED.details, reported_at = EXCLUDED.reported_at
                    WHERE client_state.reported_at <= EXCLUDED.reported_at;
                    IF NOT FOUND THEN
                        out_status := 'duplicate';
                    END IF;
                END IF;
            EXCEPTION WHEN data_exception OR integrity_constraint_violation OR program_limit_exceeded THEN
                why := SQLERRM;
            END;
        END IF;
        IF why IS NOT NULL THEN
            out_status := 'rejected: ' || why;
        END IF;
        RETURN NEXT;
    END LOOP;
    PERFORM set_config('rport.tenant_id', coalesce(prev_tenant, ''), true);
END
$$;

-- Functions are executable by PUBLIC by default; these are granted explicitly (migrate.GrantDevice).
REVOKE EXECUTE ON FUNCTION device_tenants(), device_client_auth(text), device_client(text, text),
    device_client_groups(text), device_stored_tunnels(text, text), rport_ingest_item(jsonb),
    rport_ingest_claim(jsonb, text), ingest_job_results(jsonb), ingest_measurements(jsonb),
    ingest_connection_events(jsonb), ingest_audit(jsonb), report_client_state(jsonb) FROM PUBLIC;
