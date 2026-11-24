-- Vault (server/vault, vault.db after 002_tenant): each tenant's vault status and its values.

-- id is global, as SQLite's autoincrement: the ids of deleted values are never handed out again. required_group
-- and updated_by are nullable as in SQLite; the Go code always writes them.
CREATE TABLE vault_values (
    tenant_id      text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id             bigserial   NOT NULL,
    client_id      text        NOT NULL DEFAULT '',
    required_group text        NULL,
    created_at     timestamptz NOT NULL,
    created_by     text        NOT NULL,
    updated_at     timestamptz NOT NULL,
    updated_by     text        NULL,
    key            text        NOT NULL,
    value          text        NOT NULL,
    type           text        NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE UNIQUE INDEX vault_values_unique_client_id_key ON vault_values (tenant_id, client_id, key);
CREATE INDEX vault_values_key ON vault_values (tenant_id, key);
SELECT rport_enable_rls('vault_values');

-- one status row per tenant; enc_check and dec_check are empty until the vault is set up with a passphrase.
-- dec_check holds raw bytes (a timestamp followed by a SHA-256 sum, which SQLite keeps in a TEXT column), so bytea.
CREATE TABLE vault_status (
    tenant_id text      NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id        bigserial NOT NULL,
    db_status text      NOT NULL,
    enc_check text      NULL,
    dec_check bytea     NULL,
    PRIMARY KEY (tenant_id)
);
SELECT rport_enable_rls('vault_status');
