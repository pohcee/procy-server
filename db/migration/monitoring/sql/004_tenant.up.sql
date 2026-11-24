-- Measurements can be several GB. Add the tenant in place: ADD COLUMN with a constant default
-- rewrites no rows and needs no extra disk space. The key stays (client_id, timestamp). This table
-- holds a standalone server's own metrics, and every query filters on tenant_id as well.
ALTER TABLE measurements ADD COLUMN "tenant_id" TEXT NOT NULL DEFAULT 'default';
