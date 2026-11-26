-- Operator console: the per-node view (pohcee/procy#14). What each node runs (version, roles, process start, from
-- its heartbeat), the transfer it carried per UTC day (usage_node_daily, from ingest_usage), and the operator's
-- reads of the nodes, their devices and connection events across organizations. Standalone servers have none of
-- this.

-- What the node's process reported in its heartbeat; NULL for a node on a release before this one.
ALTER TABLE nodes
    ADD COLUMN version    text        NULL CHECK (length(version) <= 64),
    ADD COLUMN roles      text[]      NULL CHECK (cardinality(roles) <= 8),
    ADD COLUMN started_at timestamptz NULL;

-- The 6-argument heartbeat a node of this release calls. It is not SECURITY DEFINER: it hands what it reports to
-- the 3-argument function (which is, and which it calls, so the caller needs EXECUTE on that one too) in the
-- transaction-local setting rport.node_report. So a device role granted it has no new SECURITY DEFINER function: a
-- device process of the previous release, whose role check refuses any it doesn't know, still starts on a database
-- migrated to this version (a rolling upgrade, or a rollback).
CREATE FUNCTION rport_node_heartbeat(p_node text, p_region text, p_peer_address text, p_version text, p_roles text[],
                                     p_started_at timestamptz) RETURNS void
    LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    PERFORM set_config('rport.node_report', jsonb_build_object('version', p_version, 'roles', to_jsonb(p_roles),
                                                               'started_at', p_started_at)::text, true);
    PERFORM rport_node_heartbeat(p_node, p_region, p_peer_address);
    PERFORM set_config('rport.node_report', '', true);
END
$$;

-- rport_node_heartbeat (3 arguments, migration 1200) records the heartbeat, and with rport.node_report set (by the
-- 6-argument form) the version, roles and process start it reports. Without it (a node of the previous release)
-- they are cleared: a node that was downgraded doesn't keep showing the newer version.
CREATE OR REPLACE FUNCTION rport_node_heartbeat(p_node text, p_region text, p_peer_address text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    r jsonb := nullif(current_setting('rport.node_report', true), '')::jsonb;
    v_version text;
    v_roles text[];
    v_started timestamptz;
BEGIN
    PERFORM rport_check_registry_args(p_node, 1);
    IF r IS NOT NULL THEN
        v_version := nullif(r ->> 'version', '');
        IF length(v_version) > 64 THEN
            RAISE EXCEPTION 'version too long' USING ERRCODE = '22023';
        END IF;
        IF jsonb_typeof(r -> 'roles') = 'array' THEN
            v_roles := ARRAY(SELECT jsonb_array_elements_text(r -> 'roles'));
            IF cardinality(v_roles) > 8 OR EXISTS (SELECT 1 FROM unnest(v_roles) AS x(role) WHERE x.role !~ '^[a-z]{1,16}$') THEN
                RAISE EXCEPTION 'invalid roles' USING ERRCODE = '22023';
            END IF;
        END IF;
        v_started := (r ->> 'started_at')::timestamptz;
    END IF;
    INSERT INTO nodes (node_id, region, peer_address, heartbeat_at, version, roles, started_at)
    VALUES (p_node, coalesce(p_region, ''), coalesce(p_peer_address, ''), now(), v_version, v_roles, v_started)
    ON CONFLICT (node_id) DO UPDATE
        SET region = EXCLUDED.region, peer_address = EXCLUDED.peer_address, heartbeat_at = EXCLUDED.heartbeat_at,
            version = EXCLUDED.version, roles = EXCLUDED.roles, started_at = EXCLUDED.started_at;
END
$$;

-- the new form gets the EXECUTE grants of the 3-argument one ("rportd migrate --grant-to/--grant-device-to" may not
-- run again before the new binary calls it); that one stays for processes still running the previous release.
REVOKE EXECUTE ON FUNCTION rport_node_heartbeat(text, text, text, text, text[], timestamptz) FROM PUBLIC;
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT DISTINCT a.grantee::regrole AS role
             FROM pg_proc p, aclexplode(p.proacl) a
             WHERE p.oid = 'rport_node_heartbeat(text, text, text)'::regprocedure
               AND a.grantee <> p.proowner AND a.grantee <> 0 AND a.privilege_type = 'EXECUTE'
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION rport_node_heartbeat(text, text, text, text, text[], timestamptz) TO %s', r.role);
    END LOOP;
END
$$;

-- usage_node_daily: the transfer metrics each node counted per UTC day, every organization together. Not
-- tenant-scoped (no organization's data: sums over all of them); written only by ingest_usage, read by the operator
-- functions below, pruned by the usage upkeep with usage_daily. Usage counted before this migration has no node.
CREATE TABLE usage_node_daily (
    node_id text   NOT NULL,
    day     date   NOT NULL,
    metric  text   NOT NULL,
    value   bigint NOT NULL CHECK (value >= 0),
    PRIMARY KEY (node_id, day, metric)
);
CREATE INDEX usage_node_daily_day ON usage_node_daily (day);

-- ingest_usage (migration 1300) as before, and the transfer metrics of an item with a node_id (a node id as the
-- registry takes it; others are ignored as before) are added to usage_node_daily too.
CREATE OR REPLACE FUNCTION ingest_usage(batch jsonb) RETURNS TABLE (out_key text, out_status text)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev_tenant text := current_setting('rport.tenant_id', true); it jsonb; d jsonb; why text;
        v_tenant text; v_hour timestamptz; v_metric text; v_value bigint; v_ceiling bigint; v_client text;
        v_day date; v_ids jsonb; v_node text;
BEGIN
    PERFORM rport_ingest_batch(batch);
    FOR it IN SELECT e FROM jsonb_array_elements(batch) AS e LOOP
        out_key := coalesce(it ->> 'key', '');
        out_status := 'applied';
        why := NULL;
        v_tenant := it ->> 'tenant_id';
        IF jsonb_typeof(it) IS DISTINCT FROM 'object' THEN
            why := 'item is not an object';
        ELSIF out_key = '' OR length(out_key) > 200 THEN
            why := 'key must have 1 to 200 characters';
        ELSIF v_tenant IS NULL OR NOT EXISTS (SELECT 1 FROM tenants WHERE id = v_tenant AND status <> 'reserved') THEN
            why := 'unknown or reserved tenant';
        END IF;
        IF why IS NULL THEN
            v_node := it ->> 'node_id';
            IF v_node IS NOT NULL AND v_node !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$' THEN
                v_node := NULL;
            END IF;
            PERFORM set_config('rport.tenant_id', v_tenant, true);
            BEGIN
                IF jsonb_typeof(coalesce(it -> 'deltas', '[]')) <> 'array'
                        OR jsonb_typeof(coalesce(it -> 'devices', '[]')) <> 'array' THEN
                    RAISE EXCEPTION 'deltas and devices must be arrays' USING ERRCODE = 'data_exception';
                END IF;
                IF jsonb_array_length(coalesce(it -> 'deltas', '[]')) > 100000 THEN
                    RAISE EXCEPTION 'at most 100000 deltas per item' USING ERRCODE = 'data_exception';
                END IF;
                IF NOT rport_ingest_claim(it, 'usage') THEN
                    out_status := 'duplicate';
                ELSE
                    FOR d IN SELECT e FROM jsonb_array_elements(coalesce(it -> 'deltas', '[]')) AS e LOOP
                        v_hour := (d ->> 'hour')::timestamptz;
                        v_metric := d ->> 'metric';
                        v_value := (d ->> 'value')::bigint;
                        v_ceiling := rport_usage_ceiling(v_metric);
                        v_client := coalesce(d ->> 'client_id', '');
                        IF v_hour IS NULL OR v_hour <> date_trunc('hour', v_hour, 'UTC') THEN
                            RAISE EXCEPTION 'hour % is not on the hour', d ->> 'hour' USING ERRCODE = 'data_exception';
                        END IF;
                        IF v_hour > now() + interval '1 hour' OR v_hour < now() - interval '7 days' THEN
                            RAISE EXCEPTION 'hour % is too far from now', v_hour USING ERRCODE = 'data_exception';
                        END IF;
                        IF v_ceiling IS NULL THEN
                            RAISE EXCEPTION 'unknown metric %', v_metric USING ERRCODE = 'data_exception';
                        END IF;
                        IF v_value IS NULL OR v_value < 0 OR v_value > v_ceiling THEN
                            RAISE EXCEPTION 'value % of % out of range', v_value, v_metric USING ERRCODE = 'data_exception';
                        END IF;
                        IF length(v_client) > 200 THEN
                            RAISE EXCEPTION 'client_id too long' USING ERRCODE = 'data_exception';
                        END IF;
                        INSERT INTO usage_hourly AS u (tenant_id, hour, metric, value)
                        VALUES (v_tenant, v_hour, v_metric, v_value)
                        ON CONFLICT (tenant_id, hour, metric) DO UPDATE SET value = CASE
                            WHEN rport_usage_is_gauge(u.metric) THEN greatest(u.value, EXCLUDED.value)
                            ELSE u.value + EXCLUDED.value END;
                        IF v_client <> '' AND v_metric IN ('tunnel_bytes_to_user', 'tunnel_bytes_to_device', 'upload_bytes_to_devices')
                                AND (EXISTS (SELECT 1 FROM client_state s WHERE s.tenant_id = v_tenant AND s.client_id = v_client)
                                     OR EXISTS (SELECT 1 FROM clients c WHERE c.tenant_id = v_tenant AND c.id = v_client)) THEN
                            INSERT INTO usage_client_daily AS u (tenant_id, day, client_id, metric, value)
                            VALUES (v_tenant, (v_hour AT TIME ZONE 'UTC')::date, v_client, v_metric, v_value)
                            ON CONFLICT (tenant_id, day, client_id, metric) DO UPDATE SET value = u.value + EXCLUDED.value;
                        END IF;
                        IF v_node IS NOT NULL AND v_value > 0
                                AND v_metric IN ('tunnel_bytes_to_user', 'tunnel_bytes_to_device', 'upload_bytes_to_devices', 'api_bytes_out') THEN
                            INSERT INTO usage_node_daily AS u (node_id, day, metric, value)
                            VALUES (v_node, (v_hour AT TIME ZONE 'UTC')::date, v_metric, v_value)
                            ON CONFLICT (node_id, day, metric) DO UPDATE SET value = u.value + EXCLUDED.value;
                        END IF;
                    END LOOP;
                    FOR d IN SELECT e FROM jsonb_array_elements(coalesce(it -> 'devices', '[]')) AS e LOOP
                        v_day := (d ->> 'day')::date;
                        v_ids := d -> 'client_ids';
                        IF v_day IS NULL OR v_day > (now() AT TIME ZONE 'UTC')::date + 1 OR v_day < (now() AT TIME ZONE 'UTC')::date - 7 THEN
                            RAISE EXCEPTION 'day % is too far from now', d ->> 'day' USING ERRCODE = 'data_exception';
                        END IF;
                        IF jsonb_typeof(v_ids) IS DISTINCT FROM 'array' OR jsonb_array_length(v_ids) > 1000000 THEN
                            RAISE EXCEPTION 'client_ids must be an array of at most 1000000 ids' USING ERRCODE = 'data_exception';
                        END IF;
                        INSERT INTO usage_device_days (tenant_id, day, client_id)
                        SELECT v_tenant, v_day, e.cid
                        FROM jsonb_array_elements_text(v_ids) AS e(cid)
                        WHERE length(e.cid) BETWEEN 1 AND 200
                          AND (EXISTS (SELECT 1 FROM client_state s WHERE s.tenant_id = v_tenant AND s.client_id = e.cid)
                               OR EXISTS (SELECT 1 FROM clients c WHERE c.tenant_id = v_tenant AND c.id = e.cid))
                        ON CONFLICT DO NOTHING;
                    END LOOP;
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

-- The operator functions below read the connection events across tenants (as 1100's cross_tenant_read: only the
-- schema owner, which they run as, and only while rport.cross_tenant_read is 'on'), by node and time.
CREATE POLICY cross_tenant_read ON client_connection_events FOR SELECT TO CURRENT_USER
    USING (current_setting('rport.cross_tenant_read', true) = 'on');
CREATE INDEX client_connection_events_node ON client_connection_events (node_id, at DESC);

-- rport_operator_nodes: one row per node of the registry, with
--   age_seconds              since its last heartbeat;
--   n_online, n_orgs         its live sessions and their organizations;
--   n_connects, n_disconnects  its connection events of the last 24 hours;
--   today_in/out, period_in/out  its transfer on the UTC day p_to - 1 and in [p_from, p_to) (in: to the devices,
--                            tunnel_bytes_to_device + upload_bytes_to_devices; out: from them, tunnel_bytes_to_user +
--                            api_bytes_out, as rport_operator_usage_days);
--   metered_since            its first day in usage_node_daily (NULL: none).
-- Only counts: no device, organization or address leaves the function (peer_address is the node's own).
CREATE FUNCTION rport_operator_nodes(p_from date, p_to date)
    RETURNS TABLE (node_id text, region text, peer_address text, version text, roles text[], started_at timestamptz,
                   heartbeat_at timestamptz, age_seconds double precision, n_online bigint, n_orgs bigint,
                   n_connects bigint, n_disconnects bigint, today_in bigint, today_out bigint, period_in bigint,
                   period_out bigint, metered_since date)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM rport_operator_reader();
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY
    WITH live AS (
        SELECT s.node_id AS nid, count(*) AS n, count(DISTINCT s.tenant_id) AS orgs
        FROM client_sessions s WHERE s.lease_until > now() GROUP BY s.node_id
    )
    -- per node (few), so the indexes by node serve the events and the transfer
    SELECT n.node_id, n.region, n.peer_address, n.version, n.roles, n.started_at, n.heartbeat_at,
           greatest(extract(epoch FROM now() - n.heartbeat_at), 0)::double precision,
           coalesce(live.n, 0), coalesce(live.orgs, 0), ev.c, ev.dc,
           coalesce(tr.tin, 0)::bigint, coalesce(tr.tout, 0)::bigint, coalesce(tr.pin, 0)::bigint,
           coalesce(tr.pout, 0)::bigint,
           (SELECT min(u.day) FROM usage_node_daily u WHERE u.node_id = n.node_id)
    FROM nodes n
    LEFT JOIN live ON live.nid = n.node_id
    CROSS JOIN LATERAL (
        SELECT count(*) FILTER (WHERE e.event = 'connected') AS c, count(*) FILTER (WHERE e.event = 'disconnected') AS dc
        FROM client_connection_events e WHERE e.node_id = n.node_id AND e.at > now() - interval '24 hours'
    ) ev
    CROSS JOIN LATERAL (
        SELECT sum(u.value) FILTER (WHERE u.day = p_to - 1 AND u.metric IN ('tunnel_bytes_to_device', 'upload_bytes_to_devices')) AS tin,
               sum(u.value) FILTER (WHERE u.day = p_to - 1 AND u.metric IN ('tunnel_bytes_to_user', 'api_bytes_out')) AS tout,
               sum(u.value) FILTER (WHERE u.metric IN ('tunnel_bytes_to_device', 'upload_bytes_to_devices')) AS pin,
               sum(u.value) FILTER (WHERE u.metric IN ('tunnel_bytes_to_user', 'api_bytes_out')) AS pout
        FROM usage_node_daily u WHERE u.node_id = n.node_id AND u.day >= p_from AND u.day < p_to
    ) tr
    ORDER BY n.node_id;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- rport_operator_unassigned_sessions: the live sessions whose node is not in the registry.
CREATE FUNCTION rport_operator_unassigned_sessions() RETURNS bigint
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_reader();
    RETURN (SELECT count(*) FROM client_sessions s
            WHERE s.lease_until > now() AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.node_id = s.node_id));
END
$$;

-- rport_operator_node_days: the transfer (in and out as rport_operator_nodes) and the connection events of node
-- p_node per UTC day in [p_from, p_to). Days without either are left out.
CREATE FUNCTION rport_operator_node_days(p_node text, p_from date, p_to date)
    RETURNS TABLE (day date, bytes_in bigint, bytes_out bigint, n_connects bigint, n_disconnects bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM rport_operator_reader();
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY
    WITH tr AS (
        SELECT u.day AS d,
               sum(u.value) FILTER (WHERE u.metric IN ('tunnel_bytes_to_device', 'upload_bytes_to_devices')) AS bin,
               sum(u.value) FILTER (WHERE u.metric IN ('tunnel_bytes_to_user', 'api_bytes_out')) AS bout
        FROM usage_node_daily u WHERE u.node_id = p_node AND u.day >= p_from AND u.day < p_to
        GROUP BY u.day
    ), ev AS (
        SELECT (e.at AT TIME ZONE 'UTC')::date AS d, count(*) FILTER (WHERE e.event = 'connected') AS c,
               count(*) FILTER (WHERE e.event = 'disconnected') AS dc
        FROM client_connection_events e
        WHERE e.node_id = p_node AND e.at >= (p_from::timestamp AT TIME ZONE 'UTC') AND e.at < (p_to::timestamp AT TIME ZONE 'UTC')
        GROUP BY 1
    )
    SELECT coalesce(tr.d, ev.d), coalesce(tr.bin, 0)::bigint, coalesce(tr.bout, 0)::bigint, coalesce(ev.c, 0)::bigint,
           coalesce(ev.dc, 0)::bigint
    FROM tr FULL JOIN ev ON ev.d = tr.d
    ORDER BY 1;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- rport_operator_node_organizations: the organizations with live sessions on node p_node, with how many.
CREATE FUNCTION rport_operator_node_organizations(p_node text)
    RETURNS TABLE (org text, org_name text, n_online bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_reader();
    RETURN QUERY
    SELECT t.id, t.name, count(*)
    FROM client_sessions s JOIN tenants t ON t.id = s.tenant_id
    WHERE s.node_id = p_node AND s.lease_until > now() AND left(t.id, 1) <> '_'
    GROUP BY t.id, t.name
    ORDER BY count(*) DESC, t.id;
END
$$;

-- rport_operator_node_events: the newest p_limit (1 to 500) connection events of node p_node, with the device's
-- name (only the name of its stored document).
CREATE FUNCTION rport_operator_node_events(p_node text, p_limit integer)
    RETURNS TABLE (at timestamptz, org text, client_id text, device_name text, event text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM rport_operator_reader();
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY
    SELECT e.at, e.tenant_id, e.client_id,
           coalesce((SELECT r.details::jsonb ->> 'name' FROM client_records r
                     WHERE r.tenant_id = e.tenant_id AND r.id = e.client_id LIMIT 1), ''),
           e.event
    FROM client_connection_events e
    WHERE e.node_id = p_node AND left(e.tenant_id, 1) <> '_'
    ORDER BY e.at DESC, e.tenant_id, e.id DESC
    LIMIT least(greatest(coalesce(p_limit, 50), 1), 500);
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- rport_operator_node_devices: a page (p_limit 1 to 500 rows from p_offset) of the devices with a live session on
-- node p_node, by organization and name: metadata only (name, version and OS of the stored document, the session,
-- the first report), never addresses, tags, labels or attributes.
CREATE FUNCTION rport_operator_node_devices(p_node text, p_limit integer, p_offset integer)
    RETURNS TABLE (org text, org_name text, id text, name text, version text, os_full_name text, os text,
                   os_family text, connected_at timestamptz, first_seen_at timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM rport_operator_reader();
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY
    SELECT t.id, t.name, s.client_id,
           coalesce(r.doc ->> 'name', ''), coalesce(r.doc ->> 'version', ''), coalesce(r.doc ->> 'os_full_name', ''),
           coalesce(r.doc ->> 'os', ''), coalesce(r.doc ->> 'os_family', ''), s.connected_at, st.first_seen_at
    FROM client_sessions s
    JOIN tenants t ON t.id = s.tenant_id
    LEFT JOIN LATERAL (SELECT x.details::jsonb AS doc FROM client_records x
                       WHERE x.tenant_id = s.tenant_id AND x.id = s.client_id LIMIT 1) r ON true
    LEFT JOIN client_state st ON st.tenant_id = s.tenant_id AND st.client_id = s.client_id
    WHERE s.node_id = p_node AND s.lease_until > now() AND left(t.id, 1) <> '_'
    ORDER BY t.id, lower(coalesce(r.doc ->> 'name', '')), s.client_id
    LIMIT least(greatest(coalesce(p_limit, 50), 1), 500) OFFSET greatest(coalesce(p_offset, 0), 0);
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

REVOKE EXECUTE ON FUNCTION rport_operator_nodes(date, date), rport_operator_unassigned_sessions(),
    rport_operator_node_days(text, date, date), rport_operator_node_organizations(text),
    rport_operator_node_events(text, integer), rport_operator_node_devices(text, integer, integer) FROM PUBLIC;

-- the operator reads go to the roles that run the other operator reads (migration 1750), so a plain "rportd
-- migrate" serves them before --grant-to runs again
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT DISTINCT a.grantee::regrole AS role
             FROM pg_proc p, aclexplode(p.proacl) a
             WHERE p.oid = 'rport_operator_usage_days(date, date)'::regprocedure
               AND a.grantee <> p.proowner AND a.grantee <> 0 AND a.privilege_type = 'EXECUTE'
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION rport_operator_nodes(date, date), rport_operator_unassigned_sessions(), '
                       'rport_operator_node_days(text, date, date), rport_operator_node_organizations(text), '
                       'rport_operator_node_events(text, integer), rport_operator_node_devices(text, integer, integer) TO %s', r.role);
    END LOOP;
END
$$;
