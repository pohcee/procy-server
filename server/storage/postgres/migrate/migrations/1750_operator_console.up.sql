-- Operator console (phase 1): when members last signed in, and the operator's reads across organizations, set-based
-- (one statement for every organization, not one per organization). Standalone servers have none of this.

-- last_login_at: the last full sign-in (password without 2FA, after 2FA, OIDC), of the account and of the
-- membership. NULL: never since this release.
ALTER TABLE accounts ADD COLUMN last_login_at timestamptz NULL;
ALTER TABLE users ADD COLUMN last_login_at timestamptz NULL;

-- rport_record_login records a full sign-in of p_username to the transaction's tenant. Returns false when the login
-- isn't a member there (nothing is written).
CREATE FUNCTION rport_record_login(p_username text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    t text := current_setting('rport.tenant_id', true);
BEGIN
    PERFORM rport_app_caller();
    UPDATE users u SET last_login_at = now() WHERE u.tenant_id = t AND u.username = p_username;
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    UPDATE accounts a SET last_login_at = now() WHERE a.username = p_username;
    RETURN true;
END
$$;

-- A sign-in is not a change the nodes' caches care about: the change feed's trigger on users (0120, deferred) skips
-- an update that changes last_login_at and nothing else. Every other update is recorded as before, also one that
-- leaves the row as it was (a password change: the users_account trigger moves it to the account). Deferred as
-- well, so it runs after the trigger it replaces exists.
INSERT INTO migrate_deferred (name, needs, ddl) VALUES ('1750 users sign-in untracked', 'users', $d$
    DROP TRIGGER IF EXISTS record_change ON users;
    CREATE TRIGGER record_change AFTER INSERT OR DELETE ON users
        FOR EACH ROW EXECUTE FUNCTION rport_record_change('users', 'username');
    CREATE TRIGGER record_change_update AFTER UPDATE ON users
        FOR EACH ROW WHEN (OLD.last_login_at IS NOT DISTINCT FROM NEW.last_login_at
                           OR (to_jsonb(OLD) - 'last_login_at') IS DISTINCT FROM (to_jsonb(NEW) - 'last_login_at'))
        EXECUTE FUNCTION rport_record_change('users', 'username');
$d$);

-- The operator functions below read these tables across tenants (as 1100's cross_tenant_read: only the schema owner,
-- which they run as, and only while rport.cross_tenant_read is 'on'). client_sessions and membership_invitations
-- don't force row-level security on the owner; the others did not have the policy yet.
DO $$
DECLARE tbl text;
BEGIN
    FOREACH tbl IN ARRAY ARRAY['users', 'client_admissions'] LOOP
        EXECUTE format('CREATE POLICY cross_tenant_read ON %I FOR SELECT TO CURRENT_USER '
                       'USING (current_setting(''rport.cross_tenant_read'', true) = ''on'')', tbl);
    END LOOP;
END
$$;

-- rport_operator_reader refuses callers outside a transaction of the system tenant (the operator code paths set it).
CREATE FUNCTION rport_operator_reader() RETURNS void LANGUAGE plpgsql STABLE AS $$
BEGIN
    IF current_setting('rport.tenant_id', true) IS DISTINCT FROM '_system' THEN
        RAISE EXCEPTION 'organizations are read across tenants only by platform operators' USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

-- rport_operator_organization_counts: one row per organization (tenants not starting with "_"), with
--   n_devices      the devices as its client limit counts them (rport_counted_clients, p_cutoff as there);
--   n_online       devices with a live session, or a client record that is connected (disconnected_at NULL);
--   n_users        members;
--   last_device    the newest device connect or disconnect: a live session's start, a disconnected_at, or now for a
--                  record that is connected without a live session (nodes with role "all");
--   last_login     the newest last_login_at of its members;
--   n_invitations  pending invitations;
--   from_signup    a completed signup created it.
-- Only metadata: no device documents, names or addresses leave the function.
CREATE FUNCTION rport_operator_organization_counts(p_cutoff timestamptz)
    RETURNS TABLE (org text, n_devices bigint, n_online bigint, n_users bigint, last_device timestamptz,
                   last_login timestamptz, n_invitations bigint, from_signup boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM rport_operator_reader();
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY
    WITH live AS (
        SELECT s.tenant_id AS tid, s.client_id AS cid, s.connected_at FROM client_sessions s WHERE s.lease_until > now()
    ), recs AS (
        SELECT r.tenant_id AS tid, r.id AS cid, r.disconnected_at FROM client_records r
    ), counted AS (
        SELECT r.tid, r.cid FROM recs r WHERE r.disconnected_at IS NULL OR p_cutoff IS NULL OR r.disconnected_at >= p_cutoff
        UNION
        SELECT l.tid, l.cid FROM live l
        UNION
        SELECT a.tenant_id, a.client_id FROM client_admissions a WHERE a.admitted_at > now() - interval '5 minutes'
    ), online AS (
        SELECT l.tid, l.cid FROM live l
        UNION
        SELECT r.tid, r.cid FROM recs r WHERE r.disconnected_at IS NULL
    ), device_times AS (
        SELECT l.tid, l.connected_at AS at FROM live l
        UNION ALL
        SELECT r.tid, r.disconnected_at FROM recs r WHERE r.disconnected_at IS NOT NULL
        UNION ALL
        SELECT r.tid, now() FROM recs r
        WHERE r.disconnected_at IS NULL AND NOT EXISTS (SELECT 1 FROM live l WHERE l.tid = r.tid AND l.cid = r.cid)
    ), d AS (
        SELECT c.tid, count(*) AS n FROM counted c GROUP BY c.tid
    ), o AS (
        SELECT x.tid, count(*) AS n FROM online x GROUP BY x.tid
    ), dt AS (
        SELECT x.tid, max(x.at) AS at FROM device_times x GROUP BY x.tid
    ), u AS (
        SELECT m.tenant_id AS tid, count(*) AS n, max(m.last_login_at) AS at FROM users m GROUP BY m.tenant_id
    ), i AS (
        SELECT v.tenant_id AS tid, count(*) AS n FROM membership_invitations v GROUP BY v.tenant_id
    ), sg AS (
        SELECT DISTINCT s.organization_id AS tid FROM signups s WHERE s.status = 'completed' AND s.organization_id IS NOT NULL
    )
    SELECT t.id, coalesce(d.n, 0), coalesce(o.n, 0), coalesce(u.n, 0), dt.at, u.at, coalesce(i.n, 0), sg.tid IS NOT NULL
    FROM tenants t
    LEFT JOIN d ON d.tid = t.id
    LEFT JOIN o ON o.tid = t.id
    LEFT JOIN dt ON dt.tid = t.id
    LEFT JOIN u ON u.tid = t.id
    LEFT JOIN i ON i.tid = t.id
    LEFT JOIN sg ON sg.tid = t.id
    WHERE left(t.id, 1) <> '_'
    ORDER BY t.id;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- rport_operator_invitations: the pending invitations of organizations (every one when p_tenant is NULL), newest
-- first, with the organization's name.
CREATE FUNCTION rport_operator_invitations(p_tenant text)
    RETURNS TABLE (org text, org_name text, login text, groups_json text, invited_by text, invited_at timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_reader();
    RETURN QUERY
    SELECT v.tenant_id, t.name, v.username, v.groups, v.invited_by, v.created_at
    FROM membership_invitations v JOIN tenants t ON t.id = v.tenant_id
    WHERE left(v.tenant_id, 1) <> '_' AND (p_tenant IS NULL OR v.tenant_id = p_tenant)
    ORDER BY v.created_at DESC, v.tenant_id, v.username;
END
$$;

-- rport_operator_usage_days: the transfer of every organization together per UTC day in [p_from, p_to) (in: to the
-- devices, tunnel_bytes_to_device + upload_bytes_to_devices; out: from the devices, tunnel_bytes_to_user +
-- api_bytes_out) and the devices that had a session that day (usage_device_days). Days without usage are left out.
CREATE FUNCTION rport_operator_usage_days(p_from date, p_to date)
    RETURNS TABLE (day date, bytes_in bigint, bytes_out bigint, devices bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM rport_operator_reader();
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY
    WITH tr AS (
        SELECT (p.hour AT TIME ZONE 'UTC')::date AS d,
               sum(p.value) FILTER (WHERE p.metric IN ('tunnel_bytes_to_device', 'upload_bytes_to_devices')) AS bin,
               sum(p.value) FILTER (WHERE p.metric IN ('tunnel_bytes_to_user', 'api_bytes_out')) AS bout
        FROM usage_periods p
        WHERE p.hour >= (p_from::timestamp AT TIME ZONE 'UTC') AND p.hour < (p_to::timestamp AT TIME ZONE 'UTC')
          AND left(p.tenant_id, 1) <> '_'
        GROUP BY 1
    ), dv AS (
        SELECT x.day AS d, count(*) AS n FROM usage_device_days x
        WHERE x.day >= p_from AND x.day < p_to AND left(x.tenant_id, 1) <> '_'
        GROUP BY x.day
    )
    SELECT coalesce(tr.d, dv.d), coalesce(tr.bin, 0)::bigint, coalesce(tr.bout, 0)::bigint, coalesce(dv.n, 0)::bigint
    FROM tr FULL JOIN dv ON dv.d = tr.d
    ORDER BY 1;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

REVOKE EXECUTE ON FUNCTION rport_record_login(text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_operator_organization_counts(timestamptz) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_operator_invitations(text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_operator_usage_days(date, date) FROM PUBLIC;
