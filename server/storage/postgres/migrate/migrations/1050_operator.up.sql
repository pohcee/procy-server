-- Organizations run by platform operators: the system tenant, device limits and the operator functions.

-- max_clients: the most devices (clients) an organization may have; NULL is unlimited. Enforced at device login by
-- the device-login work of the organization core (migrations 1000–1049), not by this migration.
-- IF NOT EXISTS: the device-login work may add the same column.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS max_clients integer CHECK (max_clients IS NULL OR max_clients >= 0);

-- "_system" holds the platform operators: its administrators manage the organizations through the operator API.
-- It has users (it is active) but never devices.
INSERT INTO tenants (id, name, status) VALUES ('_system', 'Platform operators', 'active') ON CONFLICT (id) DO NOTHING;

CREATE FUNCTION rport_refuse_system_tenant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id = '_system' THEN
        RAISE EXCEPTION 'the system tenant has no %', TG_TABLE_NAME USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER clients_auth_not_in_system_tenant
    BEFORE INSERT OR UPDATE OF tenant_id ON clients_auth
    FOR EACH ROW EXECUTE FUNCTION rport_refuse_system_tenant();
CREATE TRIGGER clients_not_in_system_tenant
    BEFORE INSERT OR UPDATE OF tenant_id ON clients
    FOR EACH ROW EXECUTE FUNCTION rport_refuse_system_tenant();

-- The application role may only read tenants. Operators change them through these functions, which run as the
-- schema owner and only inside a transaction of the system tenant (the operator code paths set it), and only for
-- organizations: ids starting with "_" are refused. EXECUTE is granted by "rportd migrate --grant-to".
CREATE FUNCTION rport_operator_check(p_id text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('rport.tenant_id', true) IS DISTINCT FROM '_system' THEN
        RAISE EXCEPTION 'organizations are managed only by platform operators' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF p_id IS NULL OR left(p_id, 1) = '_' THEN
        RAISE EXCEPTION 'organization % is reserved', p_id USING ERRCODE = 'check_violation';
    END IF;
END
$$;

CREATE FUNCTION rport_operator_add_tenant(p_id text, p_name text, p_max_clients integer) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_check(p_id);
    INSERT INTO tenants (id, name, status, max_clients) VALUES (p_id, p_name, 'active', p_max_clients);
END
$$;

-- returns false when the organization doesn't exist
CREATE FUNCTION rport_operator_set_tenant_status(p_id text, p_status text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_check(p_id);
    IF p_status NOT IN ('active', 'suspended') THEN
        RAISE EXCEPTION 'status % is not active or suspended', p_status USING ERRCODE = 'check_violation';
    END IF;
    UPDATE tenants SET status = p_status WHERE id = p_id AND status IN ('active', 'suspended');
    RETURN FOUND;
END
$$;

-- returns false when the organization doesn't exist
CREATE FUNCTION rport_operator_set_max_clients(p_id text, p_max_clients integer) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_operator_check(p_id);
    UPDATE tenants SET max_clients = p_max_clients WHERE id = p_id AND status IN ('active', 'suspended');
    RETURN FOUND;
END
$$;

REVOKE EXECUTE ON FUNCTION rport_operator_check(text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_operator_add_tenant(text, text, integer) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_operator_set_tenant_status(text, text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_operator_set_max_clients(text, integer) FROM PUBLIC;
