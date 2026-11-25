-- Billing plans, statuses and limits (billing and metering design, phases 2 and 3). Every organization has a billing
-- record (plan, billing status, the transfer policy in force), its closed months have frozen quantities, and device
-- processes read the transfer policies through device_transfer_policy(). The application role reads these tables
-- under row-level security and changes them only through the SECURITY DEFINER functions below: the operator
-- functions and the upkeep functions run only in a transaction of the system tenant; an organization's
-- administrators can set their own spending limit, nothing else.

-- org_billing: an organization's plan and billing status. No foreign key: the record is kept, detached, when the
-- organization is deleted (invoices must stay explainable).
CREATE TABLE org_billing (
    tenant_id                text        PRIMARY KEY,
    plan                     text        NOT NULL,
    status                   text        NOT NULL DEFAULT 'paying' CHECK (status IN ('paying', 'trial', 'exempt', 'comped')),
    trial_ends_at            timestamptz NULL,
    comped_until             date        NULL,
    reason                   text        NOT NULL DEFAULT '',
    exempt_limits            text        NOT NULL DEFAULT 'plan' CHECK (exempt_limits IN ('plan', 'none')),
    spending_limit_cents     bigint      NULL CHECK (spending_limit_cents IS NULL OR spending_limit_cents >= 0),
    has_payment_method       boolean     NOT NULL DEFAULT false,
    provider_customer_id     text        NOT NULL DEFAULT '',
    provider_subscription_id text        NOT NULL DEFAULT '',
    cap_lifted_until         timestamptz NULL,
    extra_transfer_bytes     bigint      NOT NULL DEFAULT 0 CHECK (extra_transfer_bytes >= 0),
    extra_transfer_period    date        NULL,
    started_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    updated_by               text        NOT NULL DEFAULT '',
    -- the transfer policy in force, written by the enforcement upkeep (rport_billing_set_policy)
    transfer_state           text        NOT NULL DEFAULT 'normal' CHECK (transfer_state IN ('normal', 'soft', 'over', 'capped')),
    transfer_state_at        timestamptz NULL,
    allowance_bytes          bigint      NOT NULL DEFAULT 0,
    used_bytes               bigint      NOT NULL DEFAULT 0,
    soft_cap_bytes           bigint      NOT NULL DEFAULT 0,
    hard_cap_bytes           bigint      NOT NULL DEFAULT 0,
    overage_cents            bigint      NOT NULL DEFAULT 0,
    tunnel_rate              bigint      NOT NULL DEFAULT 0,
    throttle_rate            bigint      NOT NULL DEFAULT 0,
    -- the device limit the upkeep last wrote to tenants.max_clients (an operator's own limit is left alone)
    plan_max_clients         integer     NULL,
    fail_open                text        NOT NULL DEFAULT '',
    -- when a period first failed open (the hard cap is held open for the rest of it, until an operator clears it)
    fail_open_at             timestamptz NULL,
    -- a record the upkeep created (billing turned on, or a new organization) has no plan chosen yet: it is metered
    -- and shown on the free plan, without limits, and its device limit is left alone, until a plan is set
    plan_unassigned          boolean     NOT NULL DEFAULT false,
    -- the status and plan before the last change and since when the current ones apply: a period closed after a
    -- change is closed with what applied at its end
    previous_status          text        NULL,
    status_since             timestamptz NULL,
    previous_plan            text        NULL,
    plan_since               timestamptz NULL
);
SELECT rport_enable_rls('org_billing');

-- billing_periods: the frozen quantities of each closed month (billing.Period). Kept 7 years, detached.
CREATE TABLE billing_periods (
    tenant_id               text             NOT NULL,
    period_start            date             NOT NULL,
    period_end              date             NOT NULL,
    billed_from             date             NOT NULL,
    proration               double precision NOT NULL,
    plan                    text             NOT NULL,
    billing_status          text             NOT NULL,
    billable_devices        bigint           NOT NULL,
    peak_devices            bigint           NOT NULL,
    device_days             bigint           NOT NULL,
    extra_devices           bigint           NOT NULL,
    tunnel_bytes_to_user    bigint           NOT NULL,
    tunnel_bytes_to_device  bigint           NOT NULL,
    upload_bytes_to_devices bigint           NOT NULL,
    transfer_bytes          bigint           NOT NULL,
    allowance_bytes         bigint           NOT NULL,
    overage_bytes           bigint           NOT NULL,
    overage_gib             bigint           NOT NULL,
    base_cents              bigint           NOT NULL,
    device_cents            bigint           NOT NULL,
    overage_cents           bigint           NOT NULL,
    total_cents             bigint           NOT NULL,
    state                   text             NOT NULL CHECK (state IN ('ready', 'held', 'not_charged', 'never_charged', 'reported')),
    closed_at               timestamptz      NOT NULL DEFAULT now(),
    approved_by             text             NULL,
    approved_at             timestamptz      NULL,
    reported_at             timestamptz      NULL,
    PRIMARY KEY (tenant_id, period_start)
);
CREATE INDEX billing_periods_state ON billing_periods (state) WHERE state IN ('ready', 'held');
SELECT rport_enable_rls('billing_periods');

-- billing_notices: the notifications sent, one per kind per period per organization.
CREATE TABLE billing_notices (
    tenant_id    text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    period_start date        NOT NULL,
    kind         text        NOT NULL,
    sent_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, period_start, kind)
);
SELECT rport_enable_rls('billing_notices');

-- The functions below (owned by the schema owner, which row-level security applies to as well) read and write
-- every organization's rows while rport.billing_all is on; they turn it on only for their own statements.
DO $$
DECLARE tbl text;
BEGIN
    FOREACH tbl IN ARRAY ARRAY['org_billing', 'billing_periods', 'billing_notices'] LOOP
        EXECUTE format('CREATE POLICY billing_functions ON %I TO CURRENT_USER '
                       'USING (current_setting(''rport.billing_all'', true) = ''on'') '
                       'WITH CHECK (current_setting(''rport.billing_all'', true) = ''on'')', tbl);
    END LOOP;
END
$$;

-- A database upgraded with a plain "rportd migrate" gives the new tables the default privileges an earlier
-- "--grant-to" set (writes included); they are taken back from every role but the owner (as migration 1000).
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT DISTINCT a.grantee::regrole AS role
             FROM pg_class c, aclexplode(c.relacl) a
             WHERE c.oid IN ('org_billing'::regclass, 'billing_periods'::regclass, 'billing_notices'::regclass)
               AND a.grantee <> c.relowner AND a.grantee <> 0
               AND a.privilege_type IN ('INSERT', 'UPDATE', 'DELETE', 'TRUNCATE')
    LOOP
        EXECUTE format('REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON org_billing, billing_periods, billing_notices FROM %s', r.role);
    END LOOP;
END
$$;

-- rport_billing_check refuses to run outside a transaction of the system tenant (the operator and upkeep code
-- paths set it), and turns on rport.billing_all for the rest of the calling function.
CREATE FUNCTION rport_billing_check() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('rport.tenant_id', true) IS DISTINCT FROM '_system' THEN
        RAISE EXCEPTION 'billing is managed only by platform operators' USING ERRCODE = 'insufficient_privilege';
    END IF;
    PERFORM set_config('rport.billing_all', 'on', true);
END
$$;

CREATE FUNCTION rport_billing_done() RETURNS void LANGUAGE sql AS $$
    SELECT set_config('rport.billing_all', '', true);
$$;

-- rport_billing_org_exists: the organization exists (not reserved, no "_" id).
CREATE FUNCTION rport_billing_org_exists(p_id text) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM tenants WHERE id = p_id AND status IN ('active', 'suspended') AND left(id, 1) <> '_')
$$;

-- rport_operator_set_plan sets the plan of an organization (creating its billing record) and its device limit
-- (tenants.max_clients; NULL is unlimited). Returns false when the organization doesn't exist.
CREATE FUNCTION rport_operator_set_plan(p_id text, p_plan text, p_max_clients integer, p_by text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_check(p_id);
    PERFORM rport_billing_check();
    IF NOT rport_billing_org_exists(p_id) THEN
        PERFORM rport_billing_done();
        RETURN false;
    END IF;
    INSERT INTO org_billing AS b (tenant_id, plan, updated_by, plan_max_clients) VALUES (p_id, p_plan, p_by, p_max_clients)
    ON CONFLICT (tenant_id) DO UPDATE SET plan = EXCLUDED.plan, updated_at = now(), updated_by = EXCLUDED.updated_by,
        plan_max_clients = EXCLUDED.plan_max_clients, plan_unassigned = false,
        previous_plan = CASE WHEN b.plan <> EXCLUDED.plan THEN b.plan ELSE b.previous_plan END,
        plan_since = CASE WHEN b.plan <> EXCLUDED.plan THEN now() ELSE b.plan_since END;
    UPDATE tenants SET max_clients = p_max_clients WHERE id = p_id;
    PERFORM rport_billing_done();
    RETURN true;
END
$$;

-- rport_operator_set_billing sets the billing status (the fields of the other statuses are cleared, except a
-- trial's end and a comped date, which later periods still prorate from). p_plan is the plan of an organization
-- without a billing record yet. Returns false when the organization doesn't exist.
CREATE FUNCTION rport_operator_set_billing(p_id text, p_status text, p_trial_ends_at timestamptz, p_comped_until date,
        p_reason text, p_exempt_limits text, p_plan text, p_by text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_check(p_id);
    PERFORM rport_billing_check();
    IF NOT rport_billing_org_exists(p_id) THEN
        PERFORM rport_billing_done();
        RETURN false;
    END IF;
    IF p_status = 'exempt' AND coalesce(p_reason, '') = '' THEN
        RAISE EXCEPTION 'exempt needs a reason' USING ERRCODE = 'check_violation';
    END IF;
    INSERT INTO org_billing (tenant_id, plan) VALUES (p_id, p_plan) ON CONFLICT (tenant_id) DO NOTHING;
    -- a trial or comped period ended early by an operator ends now: later periods are charged from today
    UPDATE org_billing SET status = p_status,
        previous_status = CASE WHEN status <> p_status THEN status ELSE previous_status END,
        status_since = CASE WHEN status <> p_status THEN now() ELSE status_since END,
        trial_ends_at = CASE WHEN p_status = 'trial' THEN p_trial_ends_at
                             WHEN trial_ends_at > now() THEN now() ELSE trial_ends_at END,
        comped_until = CASE WHEN p_status = 'comped' THEN p_comped_until
                            WHEN comped_until >= (now() AT TIME ZONE 'UTC')::date THEN (now() AT TIME ZONE 'UTC')::date - 1
                            ELSE comped_until END,
        reason = coalesce(p_reason, ''),
        exempt_limits = CASE WHEN p_status = 'exempt' THEN coalesce(nullif(p_exempt_limits, ''), 'plan') ELSE 'plan' END,
        updated_at = now(), updated_by = p_by
    WHERE tenant_id = p_id;
    PERFORM rport_billing_done();
    RETURN true;
END
$$;

-- rport_operator_billing_override lifts the hard cap until p_cap_lifted_until (NULL ends a lift) and raises the
-- allowance of the period starting p_extra_period by p_extra_bytes. Returns false when the organization doesn't exist.
CREATE FUNCTION rport_operator_billing_override(p_id text, p_cap_lifted_until timestamptz, p_extra_bytes bigint,
        p_extra_period date, p_clear_fail_open boolean, p_plan text, p_by text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_check(p_id);
    PERFORM rport_billing_check();
    IF NOT rport_billing_org_exists(p_id) THEN
        PERFORM rport_billing_done();
        RETURN false;
    END IF;
    INSERT INTO org_billing (tenant_id, plan) VALUES (p_id, p_plan) ON CONFLICT (tenant_id) DO NOTHING;
    UPDATE org_billing SET cap_lifted_until = p_cap_lifted_until, extra_transfer_bytes = coalesce(p_extra_bytes, 0),
        extra_transfer_period = p_extra_period,
        fail_open = CASE WHEN p_clear_fail_open THEN '' ELSE fail_open END,
        fail_open_at = CASE WHEN p_clear_fail_open THEN NULL ELSE fail_open_at END,
        updated_at = now(), updated_by = p_by
    WHERE tenant_id = p_id;
    PERFORM rport_billing_done();
    RETURN true;
END
$$;

-- rport_operator_approve_period releases a held period (overage review): it becomes ready to be charged.
-- Returns false when there is no such held period.
CREATE FUNCTION rport_operator_approve_period(p_id text, p_start date, p_by text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE found_it boolean;
BEGIN
    PERFORM rport_operator_check(p_id);
    PERFORM rport_billing_check();
    UPDATE billing_periods SET state = 'ready', approved_by = p_by, approved_at = now()
    WHERE tenant_id = p_id AND period_start = p_start AND state = 'held';
    found_it := FOUND;
    PERFORM rport_billing_done();
    RETURN found_it;
END
$$;

-- rport_operator_billing returns the billing records of every organization (p_id NULL) or of one.
CREATE FUNCTION rport_operator_billing(p_id text) RETURNS SETOF org_billing
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_billing_check();
    RETURN QUERY SELECT * FROM org_billing b WHERE p_id IS NULL OR b.tenant_id = p_id ORDER BY b.tenant_id;
    PERFORM rport_billing_done();
END
$$;

-- rport_operator_billing_periods returns the closed periods of one organization (p_id) or of every one, newest
-- first, optionally only those in p_state.
CREATE FUNCTION rport_operator_billing_periods(p_id text, p_state text) RETURNS SETOF billing_periods
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_billing_check();
    RETURN QUERY SELECT * FROM billing_periods p
        WHERE (p_id IS NULL OR p.tenant_id = p_id) AND (p_state IS NULL OR p.state = p_state)
        ORDER BY p.period_start DESC, p.tenant_id LIMIT 1000;
    PERFORM rport_billing_done();
END
$$;

-- rport_billing_ensure gives every organization without a billing record one on p_plan (paying, billing starting
-- now); returns how many it added.
CREATE FUNCTION rport_billing_ensure(p_plan text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE n integer;
BEGIN
    PERFORM rport_billing_check();
    INSERT INTO org_billing (tenant_id, plan, updated_by, plan_unassigned)
    SELECT t.id, p_plan, 'billing:upkeep', true FROM tenants t
    WHERE t.status IN ('active', 'suspended') AND left(t.id, 1) <> '_'
    ON CONFLICT (tenant_id) DO NOTHING;
    GET DIAGNOSTICS n = ROW_COUNT;
    PERFORM rport_billing_done();
    RETURN n;
END
$$;

-- rport_billing_transition ends a trial or comped status (the upkeep): the organization becomes paying, on p_plan
-- (the free plan without a payment method) with the device limit p_max_clients. Only from p_from, so a status an
-- operator just set is not overwritten. Returns false when nothing changed.
CREATE FUNCTION rport_billing_transition(p_id text, p_from text, p_plan text, p_max_clients integer) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE found_it boolean; prev integer;
BEGIN
    PERFORM rport_billing_check();
    SELECT plan_max_clients INTO prev FROM org_billing WHERE tenant_id = p_id;
    UPDATE org_billing SET status = 'paying', plan = p_plan, updated_at = now(), updated_by = 'billing:upkeep',
        plan_max_clients = p_max_clients, previous_status = status, status_since = now(),
        previous_plan = CASE WHEN plan <> p_plan THEN plan ELSE previous_plan END,
        plan_since = CASE WHEN plan <> p_plan THEN now() ELSE plan_since END
    WHERE tenant_id = p_id AND status = p_from;
    found_it := FOUND;
    IF found_it THEN
        -- a device limit an operator set by hand stays
        UPDATE tenants SET max_clients = p_max_clients WHERE id = p_id AND max_clients IS NOT DISTINCT FROM prev;
    END IF;
    PERFORM rport_billing_done();
    RETURN found_it;
END
$$;

-- rport_billing_set_policy writes the transfer policy the enforcement upkeep computed. The device limit follows the
-- plan (a trial without a payment method has its own) unless an operator set another one since the upkeep last
-- wrote it.
CREATE FUNCTION rport_billing_set_policy(p_id text, p_state text, p_allowance bigint, p_used bigint, p_soft bigint,
        p_hard bigint, p_overage_cents bigint, p_tunnel_rate bigint, p_throttle_rate bigint, p_max_clients integer,
        p_fail_open text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev integer; had boolean; unassigned boolean;
BEGIN
    PERFORM rport_billing_check();
    SELECT plan_max_clients, true, plan_unassigned INTO prev, had, unassigned FROM org_billing WHERE tenant_id = p_id;
    IF had THEN
        IF NOT unassigned AND prev IS DISTINCT FROM p_max_clients THEN
            UPDATE tenants SET max_clients = p_max_clients WHERE id = p_id AND max_clients IS NOT DISTINCT FROM prev;
        END IF;
        UPDATE org_billing SET transfer_state = p_state, transfer_state_at = now(), allowance_bytes = p_allowance,
            used_bytes = p_used, soft_cap_bytes = p_soft, hard_cap_bytes = p_hard, overage_cents = p_overage_cents,
            tunnel_rate = p_tunnel_rate, throttle_rate = p_throttle_rate, plan_max_clients = p_max_clients,
            fail_open = coalesce(p_fail_open, ''),
            fail_open_at = CASE WHEN coalesce(p_fail_open, '') = '' THEN NULL ELSE coalesce(fail_open_at, now()) END
        WHERE tenant_id = p_id;
    END IF;
    PERFORM rport_billing_done();
END
$$;

-- rport_billing_close_period records a closed period (billing.Period as JSON). Returns false when it was closed
-- before: a closed period's quantities are frozen.
CREATE FUNCTION rport_billing_close_period(p jsonb) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE n integer;
BEGIN
    PERFORM rport_billing_check();
    INSERT INTO billing_periods (tenant_id, period_start, period_end, billed_from, proration, plan, billing_status,
        billable_devices, peak_devices, device_days, extra_devices, tunnel_bytes_to_user, tunnel_bytes_to_device,
        upload_bytes_to_devices, transfer_bytes, allowance_bytes, overage_bytes, overage_gib, base_cents,
        device_cents, overage_cents, total_cents, state)
    VALUES (p ->> 'organization', ((p ->> 'start')::timestamptz AT TIME ZONE 'UTC')::date, ((p ->> 'end')::timestamptz AT TIME ZONE 'UTC')::date,
        ((p ->> 'billed_from')::timestamptz AT TIME ZONE 'UTC')::date, (p ->> 'proration')::double precision, p ->> 'plan', p ->> 'status',
        (p ->> 'billable_devices')::bigint, (p ->> 'peak_devices')::bigint, (p ->> 'device_days')::bigint,
        (p ->> 'extra_devices')::bigint, (p ->> 'tunnel_bytes_to_user')::bigint, (p ->> 'tunnel_bytes_to_device')::bigint,
        (p ->> 'upload_bytes_to_devices')::bigint, (p ->> 'transfer_bytes')::bigint, (p ->> 'allowance_bytes')::bigint,
        (p ->> 'overage_bytes')::bigint, (p ->> 'overage_gib')::bigint, (p ->> 'base_cents')::bigint,
        (p ->> 'device_cents')::bigint, (p ->> 'overage_cents')::bigint, (p ->> 'total_cents')::bigint, p ->> 'state')
    ON CONFLICT (tenant_id, period_start) DO NOTHING;
    GET DIAGNOSTICS n = ROW_COUNT;
    PERFORM rport_billing_done();
    RETURN n = 1;
END
$$;

-- rport_billing_claim_notice records that the notification p_kind of the period was sent; false when it was before.
CREATE FUNCTION rport_billing_claim_notice(p_id text, p_period date, p_kind text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE n integer;
BEGIN
    PERFORM rport_billing_check();
    INSERT INTO billing_notices (tenant_id, period_start, kind) VALUES (p_id, p_period, p_kind) ON CONFLICT DO NOTHING;
    GET DIAGNOSTICS n = ROW_COUNT;
    PERFORM rport_billing_done();
    RETURN n = 1;
END
$$;

-- rport_billing_newest_usage is the newest hour of usage of any tenant (the enforcement's stale-metering guard).
CREATE FUNCTION rport_billing_newest_usage() RETURNS timestamptz
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true); newest timestamptz;
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    SELECT max(hour) INTO newest FROM usage_hourly;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
    RETURN newest;
END
$$;

-- rport_billing_set_spending_limit: an organization's administrators set their own spending limit (cents per
-- period; NULL removes it), in a transaction of their tenant. Nothing else of the record can be changed this way.
-- Returns false without a billing record.
CREATE FUNCTION rport_billing_set_spending_limit(p_cents bigint, p_by text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE tid text := current_setting('rport.tenant_id', true); found_it boolean;
BEGIN
    IF tid IS NULL OR tid = '' OR left(tid, 1) = '_' THEN
        RAISE EXCEPTION 'no organization' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF p_cents IS NOT NULL AND (p_cents < 0 OR p_cents > 100000000000) THEN
        RAISE EXCEPTION 'spending limit out of range' USING ERRCODE = 'check_violation';
    END IF;
    UPDATE org_billing SET spending_limit_cents = p_cents, updated_at = now(), updated_by = p_by WHERE tenant_id = tid;
    RETURN FOUND;
END
$$;

-- device_transfer_policy returns the transfer policy in force of every organization with a billing record, for
-- device processes (and api processes): state, per-tunnel rate and throttle rate (bytes per second), and when the
-- upkeep wrote it (a cap written long ago is not applied: fail open).
CREATE FUNCTION device_transfer_policy()
    RETURNS TABLE (tenant_id text, plan text, transfer_state text, tunnel_rate bigint, throttle_rate bigint, updated_at timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.billing_all', true);
BEGIN
    PERFORM set_config('rport.billing_all', 'on', true);
    RETURN QUERY SELECT b.tenant_id, b.plan, b.transfer_state, b.tunnel_rate, b.throttle_rate,
            coalesce(b.transfer_state_at, b.updated_at)
        FROM org_billing b ORDER BY b.tenant_id;
    PERFORM set_config('rport.billing_all', coalesce(prev, ''), true);
END
$$;

REVOKE EXECUTE ON FUNCTION rport_billing_check(), rport_billing_done(), rport_billing_org_exists(text),
    rport_operator_set_plan(text, text, integer, text),
    rport_operator_set_billing(text, text, timestamptz, date, text, text, text, text),
    rport_operator_billing_override(text, timestamptz, bigint, date, boolean, text, text),
    rport_operator_approve_period(text, date, text), rport_operator_billing(text),
    rport_operator_billing_periods(text, text), rport_billing_ensure(text),
    rport_billing_transition(text, text, text, integer),
    rport_billing_set_policy(text, text, bigint, bigint, bigint, bigint, bigint, bigint, bigint, integer, text),
    rport_billing_close_period(jsonb), rport_billing_claim_notice(text, date, text), rport_billing_newest_usage(),
    rport_billing_set_spending_limit(bigint, text), device_transfer_policy() FROM PUBLIC;
