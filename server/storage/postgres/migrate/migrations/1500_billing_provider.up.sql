-- The payment provider (billing and metering design, phase 4): each organization's provider customer and
-- subscription and its dunning state, the webhook events received (each applied once), the usage reported for
-- each closed period, and the dunning notifications sent. The application role reads these tables (row-level
-- security by tenant) and changes them only through the SECURITY DEFINER functions below (EXECUTE granted by
-- "rportd migrate --grant-to"); the webhook, the reporting job and the dunning tick are their only writers.

-- billing_provider_accounts: one row per organization known to a provider. customer_id is unique per provider,
-- so a webhook's customer names exactly one organization. Exempt organizations never get a row (the service
-- refuses to create a customer for them).
CREATE TABLE billing_provider_accounts (
    tenant_id              text        PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,
    provider               text        NOT NULL CHECK (provider <> ''),
    customer_id            text        NOT NULL CHECK (customer_id <> ''),
    billing_email          text        NOT NULL DEFAULT '',
    subscription_id        text        NOT NULL DEFAULT '',
    subscription_status    text        NOT NULL DEFAULT '',
    -- the creation time of the newest event applied to the subscription: older events arriving late are ignored
    subscription_event_at  timestamptz,
    price_ids              text[]      NOT NULL DEFAULT '{}',
    has_payment_method     boolean     NOT NULL DEFAULT false,
    dunning_state          text        NOT NULL DEFAULT 'none'
        CHECK (dunning_state IN ('none', 'past_due', 'read_only', 'suspension_proposed')),
    payment_failed_at      timestamptz,
    failed_invoice         text        NOT NULL DEFAULT '',
    read_only_at           timestamptz,
    suspension_proposed_at timestamptz,
    updated_at             timestamptz NOT NULL DEFAULT now(),
    updated_by             text        NOT NULL DEFAULT '',
    UNIQUE (provider, customer_id)
);
SELECT rport_enable_rls('billing_provider_accounts');

-- billing_events: every verified webhook event, claimed once (a repeated delivery is acknowledged and ignored).
-- Not tenant data (an event may name no organization we know): no row-level security; operators read it.
CREATE TABLE billing_events (
    provider    text        NOT NULL,
    event_id    text        NOT NULL CHECK (event_id <> ''),
    type        text        NOT NULL,
    tenant_id   text,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, event_id)
);
CREATE INDEX billing_events_received_at ON billing_events (received_at);

-- billing_usage_reports: one row per (organization, metric, closed period) reported to the provider, with the
-- deterministic identifier sent. state: sent (the provider accepted it), confirmed (its summary matches),
-- mismatch (its summary differs; operators look), failed (the last send failed; retried).
CREATE TABLE billing_usage_reports (
    tenant_id       text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    period_start    timestamptz NOT NULL,
    period_end      timestamptz NOT NULL,
    metric          text        NOT NULL CHECK (metric IN ('extra_devices', 'overage_gib')),
    identifier      text        NOT NULL UNIQUE,
    customer_id     text        NOT NULL,
    price_id        text        NOT NULL,
    quantity        bigint      NOT NULL CHECK (quantity >= 0),
    state           text        NOT NULL CHECK (state IN ('sent', 'confirmed', 'mismatch', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0,
    last_attempt_at timestamptz,
    sent_at         timestamptz,
    confirmed_at    timestamptz,
    provider_value  double precision,
    last_error      text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, period_start, metric)
);
CREATE INDEX billing_usage_reports_state ON billing_usage_reports (state);
SELECT rport_enable_rls('billing_usage_reports');

-- billing_dunning_notices: one notification per kind per dunning episode (keyed by its first failure) or trial.
CREATE TABLE billing_dunning_notices (
    tenant_id text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    kind      text        NOT NULL,
    episode   text        NOT NULL,
    sent_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, kind, episode)
);
SELECT rport_enable_rls('billing_dunning_notices');

-- The background jobs and the operator view read every organization's rows inside the functions below.
DO $$
DECLARE tbl text;
BEGIN
    FOREACH tbl IN ARRAY ARRAY['billing_provider_accounts', 'billing_usage_reports'] LOOP
        EXECUTE format('CREATE POLICY cross_tenant_read ON %I FOR SELECT TO CURRENT_USER '
                       'USING (current_setting(''rport.cross_tenant_read'', true) = ''on'')', tbl);
    END LOOP;
END
$$;

-- rport_billing_org_check refuses reserved and unknown tenants.
CREATE FUNCTION rport_billing_org_check(p_tenant text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF p_tenant IS NULL OR left(p_tenant, 1) = '_'
            OR NOT EXISTS (SELECT 1 FROM tenants WHERE id = p_tenant AND status <> 'reserved') THEN
        RAISE EXCEPTION 'organization % is unknown or reserved', p_tenant USING ERRCODE = 'check_violation';
    END IF;
END
$$;

-- rport_billing_claim_event records a webhook event; false when it was received before.
CREATE FUNCTION rport_billing_claim_event(p_provider text, p_event_id text, p_type text, p_tenant text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    INSERT INTO billing_events (provider, event_id, type, tenant_id) VALUES (p_provider, p_event_id, p_type, p_tenant)
    ON CONFLICT DO NOTHING;
    RETURN FOUND;
END
$$;

-- rport_billing_customer_tenant returns the organization of a provider customer, or NULL.
CREATE FUNCTION rport_billing_customer_tenant(p_provider text, p_customer text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true); v text;
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    SELECT tenant_id INTO v FROM billing_provider_accounts WHERE provider = p_provider AND customer_id = p_customer;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
    RETURN v;
END
$$;

-- rport_billing_put_account writes an organization's provider account (every column from p_acc). A customer id
-- already linked to another organization fails with a unique violation.
CREATE FUNCTION rport_billing_put_account(p_tenant text, p_acc jsonb) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.tenant_id', true);
BEGIN
    PERFORM rport_billing_org_check(p_tenant);
    PERFORM set_config('rport.tenant_id', p_tenant, true);
    INSERT INTO billing_provider_accounts AS a (tenant_id, provider, customer_id, billing_email, subscription_id,
        subscription_status, subscription_event_at, price_ids, has_payment_method, dunning_state, payment_failed_at,
        failed_invoice, read_only_at, suspension_proposed_at, updated_at, updated_by)
    VALUES (p_tenant, p_acc ->> 'provider', p_acc ->> 'customer_id', coalesce(p_acc ->> 'billing_email', ''),
        coalesce(p_acc ->> 'subscription_id', ''), coalesce(p_acc ->> 'subscription_status', ''),
        (p_acc ->> 'subscription_event_at')::timestamptz,
        coalesce(ARRAY(SELECT jsonb_array_elements_text(coalesce(p_acc -> 'price_ids', '[]'))), '{}'),
        coalesce((p_acc ->> 'has_payment_method')::boolean, false), coalesce(p_acc ->> 'dunning_state', 'none'),
        (p_acc ->> 'payment_failed_at')::timestamptz, coalesce(p_acc ->> 'failed_invoice', ''),
        (p_acc ->> 'read_only_at')::timestamptz, (p_acc ->> 'suspension_proposed_at')::timestamptz,
        now(), coalesce(p_acc ->> 'updated_by', ''))
    ON CONFLICT (tenant_id) DO UPDATE SET provider = EXCLUDED.provider, customer_id = EXCLUDED.customer_id,
        billing_email = EXCLUDED.billing_email, subscription_id = EXCLUDED.subscription_id,
        subscription_status = EXCLUDED.subscription_status, subscription_event_at = EXCLUDED.subscription_event_at,
        price_ids = EXCLUDED.price_ids, has_payment_method = EXCLUDED.has_payment_method,
        dunning_state = EXCLUDED.dunning_state, payment_failed_at = EXCLUDED.payment_failed_at,
        failed_invoice = EXCLUDED.failed_invoice, read_only_at = EXCLUDED.read_only_at,
        suspension_proposed_at = EXCLUDED.suspension_proposed_at, updated_at = now(), updated_by = EXCLUDED.updated_by;
    PERFORM set_config('rport.tenant_id', coalesce(prev, ''), true);
END
$$;

-- rport_billing_accounts returns every organization's provider account (dunning tick, operator view).
CREATE FUNCTION rport_billing_accounts() RETURNS SETOF billing_provider_accounts
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY SELECT * FROM billing_provider_accounts ORDER BY tenant_id;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- rport_billing_put_report writes the usage report of (organization, period, metric) from p_rep.
CREATE FUNCTION rport_billing_put_report(p_tenant text, p_rep jsonb) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.tenant_id', true);
BEGIN
    PERFORM rport_billing_org_check(p_tenant);
    PERFORM set_config('rport.tenant_id', p_tenant, true);
    INSERT INTO billing_usage_reports AS r (tenant_id, period_start, period_end, metric, identifier, customer_id,
        price_id, quantity, state, attempts, last_attempt_at, sent_at, confirmed_at, provider_value, last_error)
    VALUES (p_tenant, (p_rep ->> 'period_start')::timestamptz, (p_rep ->> 'period_end')::timestamptz,
        p_rep ->> 'metric', p_rep ->> 'identifier', p_rep ->> 'customer_id', p_rep ->> 'price_id',
        (p_rep ->> 'quantity')::bigint, p_rep ->> 'state', coalesce((p_rep ->> 'attempts')::integer, 0),
        (p_rep ->> 'last_attempt_at')::timestamptz, (p_rep ->> 'sent_at')::timestamptz,
        (p_rep ->> 'confirmed_at')::timestamptz, (p_rep ->> 'provider_value')::double precision,
        coalesce(p_rep ->> 'last_error', ''))
    ON CONFLICT (tenant_id, period_start, metric) DO UPDATE SET state = EXCLUDED.state, attempts = EXCLUDED.attempts,
        last_attempt_at = EXCLUDED.last_attempt_at, sent_at = EXCLUDED.sent_at, confirmed_at = EXCLUDED.confirmed_at,
        provider_value = EXCLUDED.provider_value, last_error = EXCLUDED.last_error
    -- the quantity and identifier of a report never change once written
    WHERE r.identifier = EXCLUDED.identifier AND r.quantity = EXCLUDED.quantity;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'usage report % of % changed its quantity or identifier', p_rep ->> 'metric', p_tenant
            USING ERRCODE = 'check_violation';
    END IF;
    PERFORM set_config('rport.tenant_id', coalesce(prev, ''), true);
END
$$;

-- rport_billing_reports returns every organization's usage reports, or those of a period when p_period is set.
CREATE FUNCTION rport_billing_reports(p_period timestamptz) RETURNS SETOF billing_usage_reports
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.cross_tenant_read', true);
BEGIN
    PERFORM set_config('rport.cross_tenant_read', 'on', true);
    RETURN QUERY SELECT * FROM billing_usage_reports
        WHERE p_period IS NULL OR period_start = p_period ORDER BY period_start, tenant_id, metric;
    PERFORM set_config('rport.cross_tenant_read', coalesce(prev, ''), true);
END
$$;

-- rport_billing_claim_notice records a dunning notification; false when it was sent before.
CREATE FUNCTION rport_billing_claim_notice(p_tenant text, p_kind text, p_episode text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE prev text := current_setting('rport.tenant_id', true); ok boolean;
BEGIN
    PERFORM rport_billing_org_check(p_tenant);
    PERFORM set_config('rport.tenant_id', p_tenant, true);
    INSERT INTO billing_dunning_notices (tenant_id, kind, episode) VALUES (p_tenant, p_kind, p_episode)
    ON CONFLICT DO NOTHING;
    ok := FOUND;
    PERFORM set_config('rport.tenant_id', coalesce(prev, ''), true);
    RETURN ok;
END
$$;

REVOKE EXECUTE ON FUNCTION rport_billing_org_check(text), rport_billing_claim_event(text, text, text, text),
    rport_billing_customer_tenant(text, text), rport_billing_put_account(text, jsonb), rport_billing_accounts(),
    rport_billing_put_report(text, jsonb), rport_billing_reports(timestamptz),
    rport_billing_claim_notice(text, text, text) FROM PUBLIC;
