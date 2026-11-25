-- What the operators' signup queue shows about a signup (operator console design, Part 3), recorded when it is
-- requested: how the email was proven (the emailed link, or a Google sign-in) and whether the email domain has a
-- mail server (NULL: not looked up).
ALTER TABLE signups
    ADD COLUMN method   text    NOT NULL DEFAULT 'email' CHECK (method IN ('email', 'google')),
    ADD COLUMN mx_found boolean NULL;
-- duplicate checks of the queue: earlier signups of the same domain
CREATE INDEX signups_domain_status ON signups (tenant_id, domain, status);
