-- The change feed: one row per changed row of the tables other nodes cache, written by a trigger in the same
-- transaction as the change, so nothing can change those tables (through any repository, the importer, the CLI or
-- psql) without a feed row. Nodes follow the feed to drop what they cached (and, from phase 3, device nodes to keep
-- their local copy current).
--
-- Reading: seq is taken from a sequence before the writing transaction commits, so a row with a lower seq can
-- become visible after one with a higher seq. A reader therefore consumes only rows written by transactions older
-- than the oldest transaction still running (tx_id < pg_snapshot_xmin(pg_current_snapshot())): every such
-- transaction has ended, so all its rows are visible. It reads them ordered by (tx_id, seq) and resumes after the
-- last (tx_id, seq) it consumed: any row it hasn't seen yet belongs to a transaction that was still running (or had
-- no id yet), whose id is at least that xmin and so above the cursor. (Resuming from seq alone would skip a row
-- whose transaction started writing earlier but committed later than one already consumed.)
CREATE TABLE changes (
    seq       bigserial PRIMARY KEY,
    tx_id     xid8        NOT NULL DEFAULT pg_current_xact_id(),
    tenant_id text        NOT NULL,
    entity    text        NOT NULL,
    key       text        NOT NULL,
    op        text        NOT NULL CHECK (op IN ('insert', 'update', 'delete')),
    payload   jsonb,
    at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX changes_cursor ON changes (tx_id, seq);
CREATE INDEX changes_at ON changes (at);

-- changes_horizon holds the highest (tx_id, seq) retention removed: a reader whose cursor is below it missed rows and
-- must start again from a fresh snapshot.
CREATE TABLE changes_horizon (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    tx_id     xid8    NOT NULL DEFAULT '0',
    seq       bigint  NOT NULL DEFAULT 0
);
INSERT INTO changes_horizon DEFAULT VALUES;

-- The feed is read by every node for every tenant: row-level security is on (the application role may only read);
-- the rows are written by the SECURITY DEFINER functions below, owned by the schema owner. RLS is not forced, so the
-- owner's writes (also those of a tenant deletion cascading to tracked tables) are not refused for want of
-- rport.tenant_id.
-- The read-all policy shows the application role the keys (usernames, client ids) of every tenant's changes; the
-- role sees every tenant anyway, as it sets rport.tenant_id itself.
ALTER TABLE changes ENABLE ROW LEVEL SECURITY;
CREATE POLICY changes_read ON changes FOR SELECT USING (true);
ALTER TABLE changes_horizon ENABLE ROW LEVEL SECURITY;
CREATE POLICY changes_horizon_read ON changes_horizon FOR SELECT USING (true);

-- rport_record_change is the trigger of a tracked table: TG_ARGV[0] is the entity name readers see, TG_ARGV[1] the
-- column whose value is the key. An update that changes the key is recorded as a delete of the old key and an insert
-- of the new one. NOTIFY is delivered when the transaction commits (and not at all on rollback); readers also poll.
CREATE FUNCTION rport_record_change() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    new_key text;
    old_key text;
BEGIN
    IF TG_OP <> 'DELETE' THEN
        new_key := to_jsonb(NEW) ->> TG_ARGV[1];
    END IF;
    IF TG_OP <> 'INSERT' THEN
        old_key := to_jsonb(OLD) ->> TG_ARGV[1];
    END IF;
    IF TG_OP = 'INSERT' THEN
        INSERT INTO changes (tenant_id, entity, key, op) VALUES (NEW.tenant_id, TG_ARGV[0], new_key, 'insert');
    ELSIF TG_OP = 'DELETE' THEN
        INSERT INTO changes (tenant_id, entity, key, op) VALUES (OLD.tenant_id, TG_ARGV[0], old_key, 'delete');
    ELSIF new_key IS DISTINCT FROM old_key OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
        INSERT INTO changes (tenant_id, entity, key, op) VALUES (OLD.tenant_id, TG_ARGV[0], old_key, 'delete');
        INSERT INTO changes (tenant_id, entity, key, op) VALUES (NEW.tenant_id, TG_ARGV[0], new_key, 'insert');
    ELSE
        INSERT INTO changes (tenant_id, entity, key, op) VALUES (NEW.tenant_id, TG_ARGV[0], new_key, 'update');
    END IF;
    PERFORM pg_notify('rport_changes', '');
    RETURN NULL;
END
$$;

-- rport_track_changes records the changes of tbl (events: the trigger events, all by default) in the feed as entity,
-- keyed by key_column.
CREATE FUNCTION rport_track_changes(tbl regclass, entity text, key_column text,
                                    events text DEFAULT 'INSERT OR UPDATE OR DELETE') RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF events !~ '^(INSERT|UPDATE|DELETE)( OR (INSERT|UPDATE|DELETE))*$' THEN
        RAISE EXCEPTION 'rport_track_changes: invalid events %', events;
    END IF;
    EXECUTE format('CREATE TRIGGER record_change AFTER %s ON %s FOR EACH ROW '
                   'EXECUTE FUNCTION rport_record_change(%L, %L)', events, tbl, entity, key_column);
END
$$;

-- migrate_deferred holds DDL that needs a table a later-numbered migration creates (migration numbers are blocks
-- per workstream, not dependency order). "rportd migrate" runs rport_run_deferred after the migrations: each
-- statement runs once, as soon as the table it needs exists. A node refuses to start while one is pending.
CREATE TABLE migrate_deferred (
    name    text PRIMARY KEY,
    needs   text NOT NULL,
    ddl     text NOT NULL,
    done_at timestamptz
);

CREATE FUNCTION rport_run_deferred() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE
    r   record;
    ran integer := 0;
BEGIN
    FOR r IN SELECT name, needs, ddl FROM migrate_deferred WHERE done_at IS NULL ORDER BY name LOOP
        IF to_regclass(r.needs) IS NOT NULL THEN
            EXECUTE r.ddl;
            UPDATE migrate_deferred SET done_at = now() WHERE name = r.name;
            ran := ran + 1;
        END IF;
    END LOOP;
    RETURN ran;
END
$$;

-- The tables nodes cache, or will cache (device nodes); the server's cluster watcher names the reaction to each.
INSERT INTO migrate_deferred (name, needs, ddl) VALUES
    ('0120 track users', 'users', $d$SELECT rport_track_changes('users', 'users', 'username')$d$),
    ('0120 track user_groups', 'user_groups', $d$SELECT rport_track_changes('user_groups', 'user_groups', 'username')$d$),
    ('0120 track groups', 'groups', $d$SELECT rport_track_changes('groups', 'groups', 'name')$d$),
    ('0120 track api_tokens', 'api_tokens', $d$SELECT rport_track_changes('api_tokens', 'api_tokens', 'username')$d$),
    -- a session's last access is updated on every request; only its creation and deletion (revocation) matter
    ('0120 track api_sessions', 'api_sessions', $d$SELECT rport_track_changes('api_sessions', 'api_sessions', 'session_id', 'INSERT OR DELETE')$d$),
    ('0120 track clients_auth', 'clients_auth', $d$SELECT rport_track_changes('clients_auth', 'clients_auth', 'id')$d$),
    ('0120 track clients', 'clients', $d$SELECT rport_track_changes('clients', 'clients', 'id')$d$),
    ('0120 track stored_tunnels', 'stored_tunnels', $d$SELECT rport_track_changes('stored_tunnels', 'stored_tunnels', 'client_id')$d$),
    ('0120 track client_groups', 'client_groups', $d$SELECT rport_track_changes('client_groups', 'client_groups', 'id')$d$),
    ('0120 track schedules', 'schedules', $d$SELECT rport_track_changes('schedules', 'schedules', 'id')$d$),
    ('0120 track library_entries', 'library_entries', $d$SELECT rport_track_changes('library_entries', 'library_entries', 'id')$d$),
    ('0120 track vault_wrapped_keys', 'vault_wrapped_keys', $d$SELECT rport_track_changes('vault_wrapped_keys', 'vault_wrapped_keys', 'tenant_id')$d$);

-- rport_prune_changes removes feed rows older than keep and raises changes_horizon to the highest one removed. It
-- returns how many rows it removed.
CREATE FUNCTION rport_prune_changes(keep interval) RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    removed bigint;
    top_tx  xid8;
    top_seq bigint;
BEGIN
    -- rows of one transaction share at (its start time), so they are removed together
    -- only rows of transactions older than every running one: the horizon must never pass a transaction whose rows
    -- readers haven't seen yet (a transaction running for longer than keep)
    WITH d AS (DELETE FROM changes WHERE at < now() - keep AND tx_id < pg_snapshot_xmin(pg_current_snapshot())
               RETURNING tx_id, seq),
         n AS (SELECT count(*) AS n FROM d),
         top AS (SELECT tx_id, seq FROM d ORDER BY tx_id DESC, seq DESC LIMIT 1)
    SELECT n.n, top.tx_id, top.seq INTO removed, top_tx, top_seq FROM n LEFT JOIN top ON true;
    IF removed = 0 THEN
        RETURN 0;
    END IF;
    UPDATE changes_horizon SET tx_id = top_tx, seq = top_seq WHERE (top_tx, top_seq) > (tx_id, seq);
    RETURN removed;
END
$$;

-- Only the trigger and the nodes (through migrate.GrantApp) may use these: any other role could otherwise prune the
-- feed (forcing every node to reload) or attach the trigger function to a table of its own to forge feed rows.
REVOKE EXECUTE ON FUNCTION rport_record_change() FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_prune_changes(interval) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_track_changes(regclass, text, text, text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_run_deferred() FROM PUBLIC;
