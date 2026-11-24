-- Reserved tenants ("_unresolved": requests whose tenant can't be resolved) never have users: otherwise a login that
-- names no tenant would authenticate there. Refused here whatever code writes the row.
CREATE FUNCTION rport_refuse_reserved_tenant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM tenants WHERE id = NEW.tenant_id AND status = 'reserved') THEN
        RAISE EXCEPTION 'tenant % is reserved: it has no %', NEW.tenant_id, TG_TABLE_NAME
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER users_not_in_reserved_tenants
    BEFORE INSERT OR UPDATE OF tenant_id ON users
    FOR EACH ROW EXECUTE FUNCTION rport_refuse_reserved_tenant();
