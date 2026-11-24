-- Tenants and the global login index. These tables are not tenant-scoped and have no row-level security.

CREATE TABLE tenants (
    id         text PRIMARY KEY CHECK (id ~ '^[a-z0-9_][a-z0-9_-]{0,62}$'),
    name       text        NOT NULL DEFAULT '',
    status     text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'reserved')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- "default" is the tenant the server's own startup and background work runs in and the tenant devices connect to; a
-- server for one organization keeps everything there, as a standalone server does. "_unresolved" is where requests go whose tenant
-- can't be resolved (no or unknown credentials). It never has users, so authentication there always fails.
INSERT INTO tenants (id, name, status) VALUES ('default', 'Default', 'active'), ('_unresolved', 'Unresolved requests', 'reserved');

-- Logins are globally unique (one tenant per user, first release). Maintained by a trigger on users.
CREATE TABLE user_logins (
    username  text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE
);
CREATE INDEX user_logins_tenant ON user_logins (tenant_id);

-- rport_enable_rls turns on row-level security for a tenant-scoped table: rows are visible and writable only while
-- the transaction-local setting rport.tenant_id names their tenant. FORCE applies it to the table owner as well.
CREATE FUNCTION rport_enable_rls(tbl regclass) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
        'CREATE POLICY tenant_isolation ON %s USING (tenant_id = current_setting(''rport.tenant_id'', true)) '
        'WITH CHECK (tenant_id = current_setting(''rport.tenant_id'', true))', tbl);
END
$$;
