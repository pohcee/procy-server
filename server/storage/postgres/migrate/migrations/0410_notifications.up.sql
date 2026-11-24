-- Notification log (server/notifications/repository/sqlite, notifications.db after 002_tenant). Every state a
-- notification reaches is a row; id keeps their order, like SQLite's rowid.

CREATE TABLE notifications_log (
    tenant_id       text        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id              bigserial   NOT NULL,
    notification_id text        NOT NULL CHECK (notification_id <> ''),
    timestamp       timestamptz NOT NULL DEFAULT now(),
    content_type    text        NOT NULL DEFAULT '',
    reference_id    text        NOT NULL DEFAULT '',
    transport       text        NOT NULL DEFAULT '',
    recipients      text        NOT NULL DEFAULT '',
    state           text        NOT NULL CHECK (state <> ''),
    subject         text        NOT NULL DEFAULT '',
    body            text        NOT NULL DEFAULT '',
    out             text        NOT NULL DEFAULT '',
    err             text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX notifications_log_notification_id ON notifications_log (tenant_id, notification_id);
CREATE INDEX notifications_log_timestamp ON notifications_log (tenant_id, timestamp);
SELECT rport_enable_rls('notifications_log');
