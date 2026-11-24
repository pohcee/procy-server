DELETE FROM measurements WHERE tenant_id <> 'default';
ALTER TABLE measurements DROP COLUMN "tenant_id";
