-- The wrapped data encryption key of each tenant's vault (internal/vaultkms WrappedKeyStore). shredded marks a
-- tombstone: the key was destroyed (ciphertext NULL) and must not be created again.
CREATE TABLE vault_wrapped_keys (
    tenant_id  text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    key_name   text        NOT NULL DEFAULT '',
    ciphertext bytea       NULL,
    shredded   boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id)
);
SELECT rport_enable_rls('vault_wrapped_keys');
