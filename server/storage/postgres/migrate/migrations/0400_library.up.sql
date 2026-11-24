-- Library: entries with a linear version history (server/library, SQLite library.db after 006_tenant).
-- tags, os, changed and content hold the JSON the Go code marshals; they are never queried as JSON, so text.

CREATE TABLE library_entries (
    tenant_id      text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id             text        NOT NULL,
    name           text        NOT NULL,
    description    text        NOT NULL DEFAULT '',
    tags           text        NOT NULL DEFAULT '[]',
    os             text        NOT NULL DEFAULT '[]',
    active_version integer     NOT NULL,
    builtin_key    text        NULL,
    created_at     timestamptz NOT NULL,
    created_by     text        NOT NULL,
    updated_at     timestamptz NOT NULL,
    updated_by     text        NOT NULL,
    deleted_at     timestamptz NULL,
    deleted_by     text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);
-- a live name is unique per tenant
CREATE UNIQUE INDEX library_entries_unique_live_name ON library_entries (tenant_id, name) WHERE deleted_at IS NULL;
CREATE INDEX library_entries_builtin_key ON library_entries (tenant_id, builtin_key) WHERE builtin_key IS NOT NULL;
SELECT rport_enable_rls('library_entries');

CREATE TABLE library_entry_versions (
    tenant_id  text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    entry_id   text        NOT NULL,
    version    integer     NOT NULL,
    created_at timestamptz NOT NULL,
    created_by text        NOT NULL,
    message    text        NOT NULL DEFAULT '',
    changed    text        NOT NULL DEFAULT '[]',
    content    text        NOT NULL,
    PRIMARY KEY (tenant_id, entry_id, version)
);
SELECT rport_enable_rls('library_entry_versions');

CREATE TABLE library_meta (
    tenant_id text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    key       text NOT NULL,
    value     text NOT NULL,
    PRIMARY KEY (tenant_id, key)
);
SELECT rport_enable_rls('library_meta');
