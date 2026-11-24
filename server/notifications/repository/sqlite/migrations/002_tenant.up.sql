ALTER TABLE notifications_log ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
DROP INDEX idx_notifications_id;
CREATE INDEX idx_notifications_id ON notifications_log (tenant_id, notification_id);
DROP INDEX idx_notifications_timestamp;
CREATE INDEX idx_notifications_timestamp ON notifications_log (tenant_id, timestamp);
