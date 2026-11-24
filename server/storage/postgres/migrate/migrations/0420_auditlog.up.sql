-- Audit log (server/auditlog, auditlog.db after 002_tenant). One table, no rotation. The text columns are
-- nullable as in SQLite; the store reads them as ''. id keeps the insertion order, like SQLite's rowid.

CREATE TABLE auditlog (
    tenant_id       text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id              bigserial   NOT NULL,
    timestamp       timestamptz NOT NULL,
    application     text        NOT NULL,
    action          text        NOT NULL,
    username        text        NULL,
    remote_ip       text        NULL,
    affected_id     text        NULL,
    client_id       text        NULL,
    client_hostname text        NULL,
    request         text        NULL,
    response        text        NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX auditlog_tenant_timestamp ON auditlog (tenant_id, timestamp, application, action, affected_id);
CREATE INDEX auditlog_client_id ON auditlog (tenant_id, client_id);
CREATE INDEX auditlog_client_hostname ON auditlog (tenant_id, client_hostname);
CREATE INDEX auditlog_username ON auditlog (tenant_id, username);
CREATE INDEX auditlog_remote_ip ON auditlog (tenant_id, remote_ip);
SELECT rport_enable_rls('auditlog');
