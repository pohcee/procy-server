CREATE TABLE clients_new (
    tenant_id TEXT NOT NULL DEFAULT 'default',
    id TEXT NOT NULL,
    client_auth_id TEXT NOT NULL,
    disconnected_at DATETIME,
    details TEXT NOT NULL,
    PRIMARY KEY (tenant_id, id)
) WITHOUT ROWID;
INSERT INTO clients_new (tenant_id, id, client_auth_id, disconnected_at, details)
    SELECT 'default', id, client_auth_id, disconnected_at, details FROM clients;
DROP TABLE clients;
ALTER TABLE clients_new RENAME TO clients;

CREATE INDEX idx_disconnected_client
    ON clients (tenant_id, disconnected_at DESC, client_auth_id);

CREATE INDEX idx_disconnected_time_client
    ON clients (tenant_id, DATETIME(disconnected_at) DESC, client_auth_id);

CREATE TABLE stored_tunnels_new (
    tenant_id TEXT NOT NULL DEFAULT 'default',
    id TEXT NOT NULL,
    client_id TEXT NOT NULL,
    created_at DATETIME,
    name TEXT,
    scheme TEXT,
    remote_ip TEXT,
    remote_port NUMBER,
    acl TEXT,
    public_port NUMBER,
    further_options TEXT,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, client_id) REFERENCES clients (tenant_id, id) ON DELETE CASCADE
);
INSERT INTO stored_tunnels_new (tenant_id, id, client_id, created_at, name, scheme, remote_ip, remote_port, acl, public_port, further_options)
    SELECT 'default', id, client_id, created_at, name, scheme, remote_ip, remote_port, acl, public_port, further_options FROM stored_tunnels;
DROP TABLE stored_tunnels;
ALTER TABLE stored_tunnels_new RENAME TO stored_tunnels;
