-- A plan bought through the payment provider applies like an operator's (migration 1450, rport_operator_set_plan):
-- the record is no longer unassigned, the previous plan and the time of the change are kept, and the device limit
-- (tenants.max_clients) follows the plan. Both paths share rport_billing_apply_plan.

-- rport_billing_apply_plan puts organization p_id on p_plan (creating its billing record) with the device limit
-- p_max_clients (NULL is unlimited). Called only from the SECURITY DEFINER functions below, with rport.billing_all on.
CREATE FUNCTION rport_billing_apply_plan(p_id text, p_plan text, p_max_clients integer, p_by text) RETURNS void
    LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO org_billing AS b (tenant_id, plan, updated_by, plan_max_clients) VALUES (p_id, p_plan, p_by, p_max_clients)
    ON CONFLICT (tenant_id) DO UPDATE SET plan = EXCLUDED.plan, updated_at = now(), updated_by = EXCLUDED.updated_by,
        plan_max_clients = EXCLUDED.plan_max_clients, plan_unassigned = false,
        previous_plan = CASE WHEN b.plan <> EXCLUDED.plan THEN b.plan ELSE b.previous_plan END,
        plan_since = CASE WHEN b.plan <> EXCLUDED.plan THEN now() ELSE b.plan_since END;
    UPDATE tenants SET max_clients = p_max_clients WHERE id = p_id;
END
$$;

CREATE OR REPLACE FUNCTION rport_operator_set_plan(p_id text, p_plan text, p_max_clients integer, p_by text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_check(p_id);
    PERFORM rport_billing_check();
    IF NOT rport_billing_org_exists(p_id) THEN
        PERFORM rport_billing_done();
        RETURN false;
    END IF;
    PERFORM rport_billing_apply_plan(p_id, p_plan, p_max_clients, p_by);
    PERFORM rport_billing_done();
    RETURN true;
END
$$;

-- rport_billing_provider_state (migration 1500) with the device limit of the plan bought (p_max_clients). The plan
-- is applied (rport_billing_apply_plan) when it changes, the record is new or no plan was chosen yet; a repeated event for the same plan
-- leaves the device limit alone (an operator may have set another one). An exempt organization's plan is never
-- changed here; in the organization's own tenant (its checkout) only the customer is written. Returns false for an
-- unknown or reserved organization.
CREATE FUNCTION rport_billing_provider_state(p_id text, p_customer text, p_subscription text, p_has_pm boolean,
        p_plan text, p_max_clients integer, p_default_plan text, p_by text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE found_it boolean; sys boolean; created boolean; cur org_billing;
BEGIN
    sys := rport_billing_caller(p_id);
    IF NOT rport_billing_org_exists(p_id) THEN
        RETURN false;
    END IF;
    PERFORM set_config('rport.billing_all', 'on', true);
    INSERT INTO org_billing (tenant_id, plan, updated_by) VALUES (p_id, p_default_plan, p_by)
    ON CONFLICT (tenant_id) DO NOTHING;
    created := FOUND;
    UPDATE org_billing SET provider_customer_id = coalesce(p_customer, ''),
        provider_subscription_id = CASE WHEN sys THEN coalesce(p_subscription, '') ELSE provider_subscription_id END,
        has_payment_method = CASE WHEN sys THEN p_has_pm ELSE has_payment_method END,
        updated_at = now(), updated_by = p_by
    WHERE tenant_id = p_id
    RETURNING * INTO cur;
    found_it := FOUND;
    IF found_it AND sys AND coalesce(p_plan, '') <> '' AND cur.status <> 'exempt'
            AND (created OR cur.plan <> p_plan OR cur.plan_unassigned) THEN
        PERFORM rport_billing_apply_plan(p_id, p_plan, p_max_clients, p_by);
    END IF;
    PERFORM set_config('rport.billing_all', '', true);
    RETURN found_it;
END
$$;

-- the new function gets the EXECUTE grants of the one it replaces ("rportd migrate --grant-to" may not run again
-- before the new binary calls it); the old one stays for processes still running the previous release.
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT DISTINCT a.grantee::regrole AS role
             FROM pg_proc p, aclexplode(p.proacl) a
             WHERE p.oid = 'rport_billing_provider_state(text, text, text, boolean, text, text, text)'::regprocedure
               AND a.grantee <> p.proowner AND a.grantee <> 0 AND a.privilege_type = 'EXECUTE'
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION rport_billing_provider_state(text, text, text, boolean, text, integer, text, text) TO %s', r.role);
    END LOOP;
END
$$;

REVOKE EXECUTE ON FUNCTION rport_billing_apply_plan(text, text, integer, text),
    rport_billing_provider_state(text, text, text, boolean, text, integer, text, text) FROM PUBLIC;
