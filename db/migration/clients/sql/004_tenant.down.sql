CREATE TABLE clients_old (
    id TEXT PRIMARY KEY NOT NULL,
    client_auth_id TEXT NOT NULL,
    disconnected_at DATETIME,
    details TEXT NOT NULL
) WITHOUT ROWID;
INSERT INTO clients_old (id, client_auth_id, disconnected_at, details)
    SELECT id, client_auth_id, disconnected_at, details FROM clients WHERE tenant_id = 'default';
DROP TABLE clients;
ALTER TABLE clients_old RENAME TO clients;

CREATE INDEX idx_disconnected_client
    ON clients (disconnected_at DESC, client_auth_id);

CREATE INDEX idx_disconnected_time_client
    ON clients (DATETIME(disconnected_at) DESC, client_auth_id);

CREATE TABLE stored_tunnels_old (
    id TEXT PRIMARY KEY NOT NULL,
    client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    created_at DATETIME,
    name TEXT,
    scheme TEXT,
    remote_ip TEXT,
    remote_port NUMBER,
    acl TEXT,
    public_port NUMBER,
    further_options TEXT
);
INSERT INTO stored_tunnels_old (id, client_id, created_at, name, scheme, remote_ip, remote_port, acl, public_port, further_options)
    SELECT id, client_id, created_at, name, scheme, remote_ip, remote_port, acl, public_port, further_options FROM stored_tunnels WHERE tenant_id = 'default';
DROP TABLE stored_tunnels;
ALTER TABLE stored_tunnels_old RENAME TO stored_tunnels;
