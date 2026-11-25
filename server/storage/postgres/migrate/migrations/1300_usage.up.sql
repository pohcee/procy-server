-- Usage metering (billing and metering design, phase 1). Every node counts what each tenant uses per UTC hour and
-- flushes the deltas through ingest_usage: device processes from their outbox (kind "usage"), other nodes
-- directly. Each flushed item has a unique key, claimed in ingest_keys (kind 'usage') in the same transaction as
-- the deltas are added, so a delta is added exactly once however often an item is shipped. The cluster upkeep
-- rolls hours up into days, records the monitoring rows gauge and prunes (pgserver.Coordinator).

-- What each tenant used per UTC hour. Counters are sums; the gauges devices_peak and monitoring_rows keep the
-- highest value reported for the hour. Kept 90 days by default ([metering] hourly_retention).
CREATE TABLE usage_hourly (
    tenant_id text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    hour      timestamptz NOT NULL,
    metric    text        NOT NULL,
    value     bigint      NOT NULL CHECK (value >= 0),
    PRIMARY KEY (tenant_id, hour, metric)
);
CREATE INDEX usage_hourly_hour ON usage_hourly (hour);
SELECT rport_enable_rls('usage_hourly');

-- usage_hourly rolled up per UTC day (sums for counters, the highest value for gauges) for days that ended more
-- than two days ago, recomputed while late data may still arrive. Kept 25 months by default.
CREATE TABLE usage_daily (
    tenant_id text   NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    day       date   NOT NULL,
    metric    text   NOT NULL,
    value     bigint NOT NULL CHECK (value >= 0),
    PRIMARY KEY (tenant_id, day, metric)
);
CREATE INDEX usage_daily_day ON usage_daily (day);
SELECT rport_enable_rls('usage_daily');

-- The devices that had a session on each UTC day: a device counts for a day once, however many nodes saw it.
-- Kept 25 months by default.
CREATE TABLE usage_device_days (
    tenant_id text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    day       date NOT NULL,
    client_id text NOT NULL,
    PRIMARY KEY (tenant_id, day, client_id)
);
CREATE INDEX usage_device_days_day ON usage_device_days (day);
SELECT rport_enable_rls('usage_device_days');

-- The transfer metrics per client and UTC day, for the top devices of a report. Kept as long as usage_hourly.
CREATE TABLE usage_client_daily (
    tenant_id text   NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    day       date   NOT NULL,
    client_id text   NOT NULL,
    metric    text   NOT NULL,
    value     bigint NOT NULL CHECK (value >= 0),
    PRIMARY KEY (tenant_id, day, client_id, metric)
);
CREATE INDEX usage_client_daily_day ON usage_client_daily (day);
SELECT rport_enable_rls('usage_client_daily');

-- The operator summary reads every tenant's usage inside rport_usage_summary (as 1100's cross_tenant_read).
DO $$
DECLARE tbl text;
BEGIN
    FOREACH tbl IN ARRAY ARRAY['usage_hourly', 'usage_daily', 'usage_device_days'] LOOP
        EXECUTE format('CREATE POLICY cross_tenant_read ON %I FOR SELECT TO CURRENT_USER '
                       'USING (current_setting(''rport.cross_tenant_read'', true) = ''on'')', tbl);
    END LOOP;
END
$$;

-- usage_periods is the usage to report per day or month: the daily rollups for days that ended more than two days
-- ago, the hours after that, and the hours of older days not rolled up yet (the upkeep runs hourly, so a day that
-- just passed the cutoff, or every day while no node does the upkeep, is read from its hours). hour is the start of
-- the day for rolled-up rows.
CREATE VIEW usage_periods WITH (security_invoker = true) AS
SELECT d.tenant_id, (d.day::timestamp AT TIME ZONE 'UTC') AS hour, d.metric, d.value
FROM usage_daily d
WHERE d.day < (now() AT TIME ZONE 'UTC')::date - 2
UNION ALL
SELECT h.tenant_id, h.hour, h.metric, h.value
FROM usage_hourly h
WHERE h.hour >= (((now() AT TIME ZONE 'UTC')::date - 2)::timestamp AT TIME ZONE 'UTC')
   OR NOT EXISTS (SELECT 1 FROM usage_daily d
                  WHERE d.tenant_id = h.tenant_id AND d.day = (h.hour AT TIME ZONE 'UTC')::date);

-- rport_usage_ceiling is the highest value one delta of metric may have, NULL for a metric ingest_usage doesn't
-- take (monitoring_rows is recorded by the upkeep).
CREATE FUNCTION rport_usage_ceiling(metric text) RETURNS bigint LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE metric
        WHEN 'tunnel_bytes_to_user' THEN 10995116277760     -- 10 TiB
        WHEN 'tunnel_bytes_to_device' THEN 10995116277760
        WHEN 'upload_bytes_to_devices' THEN 10995116277760
        WHEN 'api_bytes_out' THEN 10995116277760
        WHEN 'jobs_command' THEN 1000000
        WHEN 'jobs_script' THEN 1000000
        WHEN 'jobs_library' THEN 1000000
        WHEN 'device_seconds' THEN 36000000000              -- 10 million devices for an hour
        WHEN 'devices_peak' THEN 10000000
    END
$$;

-- rport_usage_is_gauge: gauges keep the highest value of their hour, counters add up.
CREATE FUNCTION rport_usage_is_gauge(metric text) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
    SELECT metric IN ('devices_peak', 'monitoring_rows')
$$;

-- ingest_usage adds the usage a node counted. Item fields: tenant_id, key (node, process start, tenant and flush
-- sequence), node_id (optional), deltas (array of {hour, metric, value, client_id (optional, transfer metrics)})
-- and devices (array of {day (YYYY-MM-DD, UTC), client_ids}). Hours must be on the hour, at most an hour ahead and at most 7 days
-- old; days at most a day ahead and 7 days old; metrics known and values between 0 and their ceiling. The tenant
-- must exist and not be reserved (suspended tenants' usage is kept). An item whose key was claimed before is a
-- duplicate and changes nothing. Client ids unknown in the tenant are skipped (their bytes still count for the
-- tenant). The checks raise inside the item's block, so an invalid item is rejected as a whole, claim included.
CREATE FUNCTION ingest_usage(batch jsonb) RETURNS TABLE (out_key text, out_status text)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev_tenant text := current_setting('rport.tenant_id', true); it jsonb; d jsonb; why text;
        v_tenant text; v_hour timestamptz; v_metric text; v_value bigint; v_ceiling bigint; v_client text;
        v_day date; v_ids jsonb;
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

-- rport_usage_summary returns every tenant's usage between from_ts and to_ts (hours starting in the range; whole
-- days for the rolled-up part), for the operator API: sums of counters, the highest value of gauges.
CREATE FUNCTION rport_usage_summary(from_ts timestamptz, to_ts timestamptz)
    RETURNS TABLE (tenant_id text, metric text, value bigint)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY
        SELECT p.tenant_id, p.metric,
               (CASE WHEN rport_usage_is_gauge(p.metric) THEN max(p.value) ELSE sum(p.value) END)::bigint
        FROM usage_periods p
        WHERE p.hour >= from_ts AND p.hour < to_ts
        GROUP BY p.tenant_id, p.metric
        ORDER BY p.tenant_id, p.metric;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- rport_usage_device_summary returns every tenant's device-days between the days from_day (included) and to_day
-- (excluded): the sum of the daily device counts and the highest one.
CREATE FUNCTION rport_usage_device_summary(from_day date, to_day date)
    RETURNS TABLE (tenant_id text, device_days bigint, peak_day bigint)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY
        SELECT c.tenant_id, sum(c.n)::bigint, max(c.n)::bigint
        FROM (SELECT d.tenant_id, d.day, count(*) AS n FROM usage_device_days d
              WHERE d.day >= from_day AND d.day < to_day GROUP BY d.tenant_id, d.day) c
        GROUP BY c.tenant_id
        ORDER BY c.tenant_id;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

REVOKE EXECUTE ON FUNCTION ingest_usage(jsonb), rport_usage_summary(timestamptz, timestamptz),
    rport_usage_device_summary(date, date) FROM PUBLIC;
