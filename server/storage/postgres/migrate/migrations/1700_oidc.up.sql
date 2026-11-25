-- OpenID Connect sign-in ("Sign in with Google", [oidc]): pending sign-ins, one-time sign-in codes, the provider
-- identities pinned to accounts, and the sign-in policy of organizations and memberships.

-- oidc_logins: pending sign-ins (GET /oidc/login until the callback), by the SHA-256 of the state. A sign-in has no
-- tenant until it ends: not tenant data, no row-level security. Any node takes the callback.
CREATE TABLE oidc_logins (
    state_hash   text        PRIMARY KEY,
    nonce        text        NOT NULL,
    verifier     text        NOT NULL,
    binding_hash text        NOT NULL,
    return_to    text        NOT NULL DEFAULT '',
    expires_at   timestamptz NOT NULL
);
CREATE INDEX oidc_logins_expires ON oidc_logins (expires_at);

-- oidc_tickets: the one-time sign-in codes the callback hands to the web UI (POST /oidc/token), by the SHA-256 of
-- the code. Not tenant data either: the organization is picked with the code. binding_hash ties the code to the
-- browser that started the sign-in (login CSRF).
CREATE TABLE oidc_tickets (
    code_hash    text        PRIMARY KEY,
    username     text        NOT NULL,
    binding_hash text        NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz NULL
);
CREATE INDEX oidc_tickets_expires ON oidc_tickets (expires_at);

-- account_identities: the provider identity (issuer, subject) pinned to an account at its first sign-in. Matched on
-- first afterwards, so a different provider account that later gets the same email can't take the account over.
-- One identity per account and issuer. Written only through rport_oidc_resolve and rport_oidc_unlink; deleted with
-- the account, and when the account is renamed (the pin was for the old login's owner).
CREATE TABLE account_identities (
    issuer        text        NOT NULL,
    subject       text        NOT NULL,
    username      text        NOT NULL REFERENCES accounts (username) ON DELETE CASCADE,
    email         text        NOT NULL DEFAULT '',
    linked_at     timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz NULL,
    PRIMARY KEY (issuer, subject),
    CONSTRAINT account_identities_one_per_issuer UNIQUE (username, issuer)
);

CREATE FUNCTION rport_account_rename_unpins() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.username IS DISTINCT FROM OLD.username THEN
        DELETE FROM account_identities WHERE username = OLD.username;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER accounts_rename_unpins BEFORE UPDATE OF username ON accounts
    FOR EACH ROW EXECUTE FUNCTION rport_account_rename_unpins();

-- the email of a sign-in is matched against logins ignoring case
CREATE INDEX accounts_lower_username ON accounts (lower(username));

-- require_oidc: the organization's members sign in through OIDC only (set by its administrators or operators).
ALTER TABLE tenants ADD COLUMN require_oidc boolean NOT NULL DEFAULT false;

-- sign_in: how the member may sign in to this organization: password, oidc or both (default).
ALTER TABLE users ADD COLUMN sign_in text NOT NULL DEFAULT 'both' CHECK (sign_in IN ('both', 'password', 'oidc'));

-- rport_oidc_resolve maps a verified sign-in to an account: the account the identity is pinned to ('pinned'), else
-- the one account whose login equals p_email ignoring case, which it pins the identity to ('linked'). 'unknown': no
-- such account (none is created); 'ambiguous': several logins match; 'mismatch': the matching account is pinned to
-- another identity of the issuer; 'unlinkable': no pin and p_linkable is false (the provider doesn't vouch that the
-- email is still controlled by the account, see oidc.md). A sign-in whose email is the login proves the mailbox:
-- email_verified_at is set.
CREATE FUNCTION rport_oidc_resolve(p_issuer text, p_subject text, p_email text, p_linkable boolean, OUT login text, OUT outcome text)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    n integer;
BEGIN
    PERFORM rport_app_caller();
    SELECT i.username INTO login FROM account_identities i WHERE i.issuer = p_issuer AND i.subject = p_subject;
    IF FOUND THEN
        UPDATE account_identities i SET last_login_at = now(), email = p_email
            WHERE i.issuer = p_issuer AND i.subject = p_subject;
        IF lower(login) = lower(p_email) THEN
            UPDATE accounts a SET email_verified_at = coalesce(a.email_verified_at, now()) WHERE a.username = login;
        END IF;
        outcome := 'pinned';
        RETURN;
    END IF;
    IF NOT p_linkable THEN
        login := NULL;
        outcome := 'unlinkable';
        RETURN;
    END IF;
    IF coalesce(p_email, '') = '' THEN
        login := NULL;
        outcome := 'unknown';
        RETURN;
    END IF;
    SELECT count(*), min(a.username) INTO n, login FROM accounts a WHERE lower(a.username) = lower(p_email);
    IF n = 0 THEN
        login := NULL;
        outcome := 'unknown';
        RETURN;
    ELSIF n > 1 THEN
        login := NULL;
        outcome := 'ambiguous';
        RETURN;
    END IF;
    PERFORM 1 FROM accounts a WHERE a.username = login FOR UPDATE;
    -- a concurrent first sign-in of the same identity may have pinned it meanwhile
    IF EXISTS (SELECT 1 FROM account_identities i WHERE i.issuer = p_issuer AND i.subject = p_subject AND i.username = login) THEN
        outcome := 'pinned';
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM account_identities i WHERE i.username = login AND i.issuer = p_issuer) THEN
        login := NULL;
        outcome := 'mismatch';
        RETURN;
    END IF;
    INSERT INTO account_identities (issuer, subject, username, email, last_login_at) VALUES (p_issuer, p_subject, login, p_email, now());
    UPDATE accounts a SET email_verified_at = coalesce(a.email_verified_at, now()) WHERE a.username = login;
    outcome := 'linked';
END
$$;

-- rport_oidc_unlink removes the identities pinned to a member of the transaction's tenant (row-level security on
-- users limits the check to it). Returns false when the login isn't a member there.
CREATE FUNCTION rport_oidc_unlink(p_username text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM rport_app_caller();
    IF NOT EXISTS (SELECT 1 FROM users WHERE username = p_username AND tenant_id = current_setting('rport.tenant_id', true)) THEN
        RETURN false;
    END IF;
    DELETE FROM account_identities WHERE username = p_username;
    RETURN true;
END
$$;

-- rport_set_require_oidc sets require_oidc of an organization: from a transaction of that organization (its
-- administrators; the application checks the group) or of the system tenant (operators). Reserved tenants ("_")
-- are refused: the system tenant always requires OIDC. Returns false when the organization doesn't exist.
CREATE FUNCTION rport_set_require_oidc(p_tenant text, p_value boolean) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    t text := current_setting('rport.tenant_id', true);
BEGIN
    PERFORM rport_app_caller();
    IF t IS DISTINCT FROM p_tenant AND t IS DISTINCT FROM '_system' THEN
        RAISE EXCEPTION 'the sign-in policy of % is set by its administrators or platform operators', p_tenant
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF p_tenant IS NULL OR left(p_tenant, 1) = '_' THEN
        RAISE EXCEPTION 'organization % is reserved', p_tenant USING ERRCODE = 'check_violation';
    END IF;
    UPDATE tenants SET require_oidc = p_value WHERE id = p_tenant;
    RETURN FOUND;
END
$$;

REVOKE EXECUTE ON FUNCTION rport_oidc_resolve(text, text, text, boolean) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_oidc_unlink(text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION rport_set_require_oidc(text, boolean) FROM PUBLIC;
