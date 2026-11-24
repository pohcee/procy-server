-- Clients, their stored tunnels, and client groups (OSS SQLite schemas db/migration/clients and
-- db/migration/client_groups after their tenant migrations). Text columns the API filters or sorts on use the "C"
-- collation, which orders bytewise like SQLite's default BINARY collation.

-- details is the JSON document the OSS store writes (clients.EncodeStoredClient); it is never queried as JSON.
CREATE TABLE clients (
    tenant_id       text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id              text NOT NULL,
    client_auth_id  text NOT NULL,
    disconnected_at timestamptz,
    details         text NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX clients_disconnected ON clients (tenant_id, disconnected_at DESC, client_auth_id);
SELECT rport_enable_rls('clients');

-- further_options is a JSON document written and read as text.
CREATE TABLE stored_tunnels (
    tenant_id       text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id              text COLLATE "C" NOT NULL,
    client_id       text NOT NULL,
    created_at      timestamptz,
    name            text COLLATE "C",
    scheme          text COLLATE "C",
    remote_ip       text COLLATE "C",
    remote_port     integer,
    acl             text,
    public_port     integer,
    further_options text,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, client_id) REFERENCES clients (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX stored_tunnels_client ON stored_tunnels (tenant_id, client_id);
SELECT rport_enable_rls('stored_tunnels');

-- params and allowed_user_groups are JSON documents written and read as text.
CREATE TABLE client_groups (
    tenant_id           text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id                  text COLLATE "C" NOT NULL,
    description         text COLLATE "C" NOT NULL,
    params              text NOT NULL,
    allowed_user_groups text NOT NULL DEFAULT '[]',
    PRIMARY KEY (tenant_id, id)
);
SELECT rport_enable_rls('client_groups');
