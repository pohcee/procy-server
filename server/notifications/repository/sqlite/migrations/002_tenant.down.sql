DELETE FROM notifications_log WHERE tenant_id != 'default';
DROP INDEX idx_notifications_id;
CREATE INDEX idx_notifications_id ON notifications_log (notification_id);
DROP INDEX idx_notifications_timestamp;
CREATE INDEX idx_notifications_timestamp ON notifications_log (timestamp);
ALTER TABLE notifications_log DROP COLUMN tenant_id;
