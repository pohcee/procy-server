package signup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/server/operator"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

// day is the window of the rate limits.
const day = 24 * time.Hour

// approvalWait is how long a verified signup waits for an operator before it expires.
const approvalWait = 30 * 24 * time.Hour

// Options are the dependencies of a Service. Captcha, Domains and Shredder may be nil.
type Options struct {
	Config    chconfig.SignupConfig
	Passwords PasswordRules
	Store     Store
	Orgs      operator.Service
	Accounts  Accounts
	Plans     Plans
	// TrialDays is shown to the signup page (BillingPlans.TrialDays; 0: no trial).
	TrialDays int
	Mailer    Mailer
	Captcha   Captcha
	Domains   *Domains
	// LookupMX, when set, is looked up for every signup request to show the operators whether the domain has a mail
	// server (Signup.MXFound); refusing domains without one is Domains' (check_mx).
	LookupMX func(ctx context.Context, domain string) error
	// GoogleSignup: "Continue with Google" is offered ([oidc] on); GoogleSignupURL starts it.
	GoogleSignup bool
	Shredder     Shredder
	Log          *logger.Logger
	Now          func() time.Time
}

// Service runs signups and deletions.
type Service struct {
	o   Options
	now func() time.Time
}

// New returns the signup service.
func New(o Options) *Service {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	if o.Log == nil {
		o.Log = logger.NewLogger("signup", logger.LogOutput{File: os.Stderr}, logger.LogLevelError)
	}
	return &Service{o: o, now: now}
}

// PublicConfig is what the signup page needs to know (GET /signup).
type PublicConfig struct {
	Enabled           bool   `json:"enabled"`
	Captcha           string `json:"captcha"`
	CaptchaSiteKey    string `json:"captcha_site_key"`
	RequireApproval   bool   `json:"require_approval"`
	PasswordMinLength int    `json:"password_min_length"`
	TrialDays         int    `json:"trial_days"`
	// GoogleSignup: the page offers "Continue with Google" (GET GoogleSignupURL?organization=…): the Google account's
	// verified email replaces the emailed link.
	GoogleSignup      bool   `json:"google_signup"`
	GoogleSignupURL   string `json:"google_signup_url,omitempty"`
	GoogleButtonLabel string `json:"google_button_label,omitempty"`
}

// GoogleSignupURL is the route that starts a signup with Google (the server package serves it with [oidc]).
const GoogleSignupURL = "/api/v1/oidc/signup"

// PublicConfig returns the settings the signup page shows.
func (s *Service) PublicConfig() PublicConfig {
	c := s.o.Config
	pc := PublicConfig{
		Enabled:           true,
		Captcha:           c.Captcha,
		CaptchaSiteKey:    c.CaptchaSiteKey,
		RequireApproval:   c.RequireApproval,
		PasswordMinLength: s.o.Passwords.MinLength,
		TrialDays:         s.o.TrialDays,
	}
	if s.o.GoogleSignup {
		pc.GoogleSignup, pc.GoogleSignupURL, pc.GoogleButtonLabel = true, GoogleSignupURL, "Continue with Google"
	}
	return pc
}

// CheckOrganizationName checks the name of a new organization as a signup does; it returns it trimmed.
func CheckOrganizationName(name string) (string, error) { return checkOrgName(name) }

// lookupMX records whether domain has a mail server, within mxBudget; nil without Options.LookupMX.
func (s *Service) lookupMX(ctx context.Context, domain string) *bool {
	if s.o.LookupMX == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, mxBudget)
	defer cancel()
	found := s.o.LookupMX(ctx, domain) == nil
	return &found
}

// admit checks the email and the organization name of a signup request.
func admit(email, organization string) (addr, domain, org string, err error) {
	if addr, domain, err = normalizeEmail(email); err != nil {
		return "", "", "", err
	}
	if org, err = checkOrgName(organization); err != nil {
		return "", "", "", err
	}
	return addr, domain, org, nil
}

// limited checks the domain lists and the rate limits of a signup request.
func (s *Service) limited(ctx context.Context, addr, domain, remoteIP string, now time.Time) error {
	if err := s.o.Domains.Check(ctx, domain); err != nil {
		return err
	}
	n, err := s.o.Store.CountSince(ctx, now.Add(-day), remoteIP, domain, addr)
	if err != nil {
		return err
	}
	c := s.o.Config
	domainLimit := c.MaxPerDomainPerDay
	if c.UnlimitedDomain(domain) {
		domainLimit = -1
	}
	if over(n.IP, c.MaxPerIPPerDay) || over(n.Domain, domainLimit) || over(n.Email, c.MaxPerEmailPerDay) {
		s.o.Log.Infof("signup refused (rate limit): %s from %s (ip %d, domain %d, email %d today)", addr, remoteIP, n.IP, n.Domain, n.Email)
		return ErrRateLimited
	}
	return nil
}

// Request is a signup as the visitor gives it. The password is chosen when verifying (Verify), so that whoever
// asks for a signup with someone else's email never knows the password of the account it creates.
type Request struct {
	Email        string `json:"email"`
	Organization string `json:"organization"`
	Captcha      string `json:"captcha"`
}

func signupActor(email, ip string) operator.Actor {
	return operator.Actor{Username: "signup:" + email, RemoteIP: ip}
}

// checkOrgName refuses names that are empty, too long or have control characters (they go into emails).
func checkOrgName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxOrgName {
		return "", fmt.Errorf("%w: the organization name must be 1 to %d characters", ErrInvalid, maxOrgName)
	}
	for _, r := range name {
		if unicode.IsControl(r) || !unicode.IsPrint(r) && r != ' ' {
			return "", fmt.Errorf("%w: the organization name must not contain control characters", ErrInvalid)
		}
	}
	return name, nil
}

// Request checks a signup and sends the verification email. The result is the same whether or not the email is
// already a user (that one gets a notice instead of a link), and both take the same work, so it doesn't reveal
// accounts.
func (s *Service) Request(ctx context.Context, in Request, remoteIP string) error {
	addr, domain, org, err := admit(in.Email, in.Organization)
	if err != nil {
		return err
	}
	if s.o.Captcha != nil {
		if err := s.o.Captcha.Verify(ctx, in.Captcha, remoteIP); err != nil {
			return err
		}
	}
	now := s.now()
	if err := s.limited(ctx, addr, domain, remoteIP, now); err != nil {
		return err
	}
	c := s.o.Config

	acct, err := s.o.Accounts.Account(ctx, addr)
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	row := Signup{ID: id, Email: addr, Domain: domain, RemoteIP: remoteIP, OrgName: org, CreatedAt: now, ExpiresAt: now.Add(c.TokenTTL),
		Status: StatusPending, TokenHash: &hash, Method: MethodEmail, MXFound: s.lookupMX(ctx, domain)}
	var m mail
	switch {
	case !acct.Exists:
		m = verifyMail(c.PublicURL, token, org, c.TokenTTL, false)
	case acct.TakeOver():
		// an administrator made an account of this address that nobody proved to own: the mailbox owner may take it
		m = verifyMail(c.PublicURL, token, org, c.TokenTTL, true)
	default:
		row.Status, row.TokenHash = StatusExisting, nil
		m = existingMail(c.PublicURL)
	}
	if err := s.o.Store.Insert(ctx, row); err != nil {
		return err
	}
	if err := s.audit(ctx, signupActor(addr, remoteIP), AuditApplicationSignup, AuditActionRequest, id, map[string]string{"organization": org}); err != nil {
		return err
	}
	return s.send(ctx, addr, m)
}

func over(n, limit int) bool { return limit > 0 && n >= limit }

// send mails m to a person who signs up (or an organization's administrators), with [signup] email_notice.
func (s *Service) send(ctx context.Context, to string, m mail) error {
	if n := s.o.Config.EmailNotice; n != "" {
		m.body = strings.TrimRight(m.body, "\n") + "\n\n-- \n" + n + "\n"
	}
	return s.sendRaw(ctx, to, m)
}

func (s *Service) sendRaw(ctx context.Context, to string, m mail) error {
	if err := s.o.Mailer.Send(ctx, to, m.subject, m.body); err != nil {
		s.o.Log.Errorf("signup: send %q to %s: %v", m.subject, to, err)
		return ErrMail
	}
	return nil
}

func (s *Service) audit(ctx context.Context, by operator.Actor, application, action, id string, request any) error {
	return s.o.Store.Audit(ctx, AuditEntry{Time: s.now(), Actor: by, Application: application, Action: action, ID: id, Request: request})
}

// Result is the outcome of a verified signup.
type Result struct {
	// Status is "completed" (the organization exists) or "awaiting_approval".
	Status string `json:"status"`
	// Organization is the new organization's id (completed only).
	Organization     string     `json:"organization,omitempty"`
	OrganizationName string     `json:"organization_name"`
	Username         string     `json:"username"`
	Plan             *PlanStart `json:"plan,omitempty"`
	// CheckoutRequired: the UI sends the user to the payment provider's checkout first (never for a trial).
	CheckoutRequired bool `json:"checkout_required"`
}

// Verify takes the token of a verification link (single use) and the password the user chooses, and creates the
// organization, or holds it for an operator with [signup] require_approval.
func (s *Service) Verify(ctx context.Context, token, password, remoteIP string) (*Result, error) {
	if token == "" {
		return nil, ErrToken
	}
	hash := hashToken(token)
	now := s.now()
	row, err := s.o.Store.ClaimToken(ctx, hash, StatusPending, now)
	if err != nil {
		return nil, err
	}
	// a refused password or a failure on the way may be retried with the same link until it expires
	restore := func() { _ = s.o.Store.RestoreToken(ctx, row.ID, hash) }
	if err := s.o.Passwords.check(password, row.Email); err != nil {
		restore()
		return nil, err
	}
	if row.PasswordHash, err = users.GenerateTokenHash(password); err != nil {
		restore()
		return nil, err
	}
	by := signupActor(row.Email, remoteIP)
	if s.o.Config.RequireApproval {
		if err := s.o.Store.SetStatus(ctx, row.ID, Update{Status: StatusAwaitingApproval, VerifiedAt: &now, ExpiresAt: ptr(approvalExpiry(now)),
			PasswordHash: row.PasswordHash}); err != nil {
			restore()
			return nil, err
		}
		if err := s.audit(ctx, by, AuditApplicationSignup, AuditActionVerify, row.ID, nil); err != nil {
			return nil, err
		}
		_ = s.send(ctx, row.Email, awaitingMail(row.OrgName))
		s.notifyOperators(ctx, row)
		return &Result{Status: StatusAwaitingApproval, OrganizationName: row.OrgName, Username: row.Email}, nil
	}
	res, err := s.complete(ctx, row, by, Update{VerifiedAt: &now})
	if err != nil {
		if !errors.Is(err, ErrConflict) {
			restore()
		}
		return nil, err
	}
	return res, nil
}

// complete creates the organization of a verified signup (row.PasswordHash set) with its first administrator and
// its plan.
func (s *Service) complete(ctx context.Context, row *Signup, by operator.Actor, u Update) (*Result, error) {
	acct, err := s.o.Accounts.Account(ctx, row.Email)
	if err != nil {
		return nil, err
	}
	if acct.Exists && !acct.TakeOver() {
		now := s.now()
		_ = s.o.Store.SetStatus(ctx, row.ID, Update{Status: StatusExisting, VerifiedAt: u.VerifiedAt, DecidedAt: &now, Reason: "the email has an account", ClearPassword: true})
		return nil, fmt.Errorf("%w: %s has an account; sign in instead", ErrConflict, row.Email)
	}
	var org *operator.Organization
	for range 3 {
		id, err := organizationID(row.OrgName)
		if err != nil {
			return nil, err
		}
		org, err = s.o.Orgs.Create(ctx, by, operator.NewOrganization{ID: id, Name: row.OrgName})
		if errors.Is(err, operator.ErrExists) {
			continue
		}
		if err != nil {
			// the tenant may exist already (a later step of Create failed): don't leave it behind
			if _, gerr := s.o.Orgs.Get(ctx, id); gerr == nil {
				if derr := s.discard(ctx, by, id); derr != nil {
					s.o.Log.Errorf("signup %s: delete the half-created organization %s: %v", row.ID, id, derr)
				}
			}
			return nil, err
		}
		break
	}
	if org == nil {
		return nil, errors.New("signup: no free organization id")
	}
	if err := s.setupOrganization(ctx, row, by, org.ID, acct.Exists); err != nil {
		s.o.Log.Errorf("signup %s: organization %s created but not set up, deleting it: %v", row.ID, org.ID, err)
		if derr := s.discard(ctx, by, org.ID); derr != nil {
			s.o.Log.Errorf("signup %s: delete the organization %s: %v", row.ID, org.ID, derr)
		}
		return nil, err
	}
	plan, err := s.o.Plans.StartOrganization(ctx, by, org.ID)
	if err != nil {
		// the organization works; its plan is set by an operator
		s.o.Log.Errorf("signup %s: organization %s created, but its plan could not be set: %v", row.ID, org.ID, err)
	}
	now := s.now()
	u.Status, u.OrganizationID, u.ClearPassword = StatusCompleted, org.ID, true
	if u.DecidedAt == nil {
		u.DecidedAt = &now
	}
	if err := s.o.Store.SetStatus(ctx, row.ID, u); err != nil {
		s.o.Log.Errorf("signup %s: organization %s created, but the signup could not be marked completed: %v", row.ID, org.ID, err)
	}
	if err := s.audit(ctx, by, AuditApplicationSignup, AuditActionCreate, org.ID, map[string]string{"signup": row.ID, "organization": row.OrgName}); err != nil {
		s.o.Log.Errorf("signup %s: audit: %v", row.ID, err)
	}
	res := &Result{Status: StatusCompleted, Organization: org.ID, OrganizationName: org.Name, Username: row.Email, Plan: plan}
	if plan != nil {
		res.CheckoutRequired = plan.CheckoutRequired
	}
	return res, nil
}

// setupOrganization makes the signup's email the administrator of org: a new account with the chosen password
// (AddAdmin), or the unproven account of the address taken over (the take-over joins it, atomically).
func (s *Service) setupOrganization(ctx context.Context, row *Signup, by operator.Actor, org string, existing bool) error {
	if existing {
		ok, err := s.o.Accounts.TakeOver(ctx, row.Email, row.PasswordHash, org)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s has an account; sign in instead", ErrConflict, row.Email)
		}
		s.o.Log.Infof("signup %s: the owner of %s took over its unproven account", row.ID, row.Email)
		if err := s.audit(ctx, by, AuditApplicationSignup, AuditActionTakeOver, row.Email, map[string]string{"signup": row.ID, "organization": org}); err != nil {
			s.o.Log.Errorf("signup %s: audit: %v", row.ID, err)
		}
		return nil
	}
	a, err := s.o.Orgs.AddAdmin(ctx, by, org, operator.NewAdmin{Username: row.Email, TwoFASendTo: row.Email})
	if err != nil {
		return err
	}
	if a.Pending {
		// an account of the address appeared meanwhile: it is not the one this signup proved
		return fmt.Errorf("%w: %s got an account meanwhile; try again", ErrConflict, row.Email)
	}
	// a Google signup has no password: its generated one is never told to anyone, it signs in with Google
	if row.PasswordHash != "" {
		if err := s.o.Accounts.SetPasswordHash(ctx, org, row.Email, row.PasswordHash); err != nil {
			return err
		}
	}
	return s.o.Accounts.MarkVerified(ctx, row.Email)
}

// discard deletes an organization created by a signup that could not be finished.
func (s *Service) discard(ctx context.Context, by operator.Actor, org string) error {
	if _, err := s.o.Orgs.SetStatus(ctx, by, org, operator.StatusSuspended); err != nil {
		return err
	}
	// a zero DeleteAfter is the database's now(): due at once by the clock rport_signup_delete_tenant uses
	if _, err := s.o.Store.AddDeletion(ctx, Deletion{OrganizationID: org, State: DeletionPending, RequestedAt: s.now(), RequestedBy: by.Username,
		Reason: "signup could not be completed", PreviousStatus: operator.StatusActive}); err != nil {
		return err
	}
	return s.runDeletion(ctx, org, by)
}

// List returns the newest signups (operators), with the queue's facts.
func (s *Service) List(ctx context.Context, status string, limit int) ([]Signup, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out, err := s.o.Store.List(ctx, status, limit)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Method == "" {
			out[i].Method = MethodEmail
		}
		if IsFreeMail(out[i].Domain) {
			out[i].FreeMail, out[i].DomainCustomers, out[i].DomainDeniedBefore = true, 0, 0
		}
	}
	return out, nil
}

// notifyOperators mails [signup] notify_email that row waits for approval; a failure is only logged.
func (s *Service) notifyOperators(ctx context.Context, row *Signup) {
	to := s.o.Config.NotifyEmail
	if to == "" {
		return
	}
	_ = s.sendRaw(ctx, to, notifyMail(s.o.Config.PublicURL, row, IsFreeMail(row.Domain)))
}

// RequestVerified is a signup whose email the caller proved (a Google sign-in, MethodGoogle): the checks of
// Request (no captcha: the provider's sign-in stands for it), then no link: the signup waits for approval at once,
// or, without [signup] require_approval, the organization is created (no password: the account signs in with
// Google). ErrAccountExists when the email has a proven account.
func (s *Service) RequestVerified(ctx context.Context, email, organization, method, remoteIP string) (*Result, error) {
	addr, domain, org, err := admit(email, organization)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if err := s.limited(ctx, addr, domain, remoteIP, now); err != nil {
		return nil, err
	}
	acct, err := s.o.Accounts.Account(ctx, addr)
	if err != nil {
		return nil, err
	}
	if acct.Exists && !acct.TakeOver() {
		return nil, ErrAccountExists
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	by := signupActor(addr, remoteIP)
	row := &Signup{ID: id, Email: addr, Domain: domain, RemoteIP: remoteIP, OrgName: org, CreatedAt: now, VerifiedAt: &now,
		Method: method, MXFound: s.lookupMX(ctx, domain)}
	if s.o.Config.RequireApproval {
		row.Status, row.ExpiresAt = StatusAwaitingApproval, approvalExpiry(now)
	} else {
		// held by this request while the organization is created
		row.Status, row.ExpiresAt = StatusCreating, now.Add(s.o.Config.TokenTTL)
	}
	if err := s.o.Store.Insert(ctx, *row); err != nil {
		return nil, err
	}
	if err := s.audit(ctx, by, AuditApplicationSignup, AuditActionVerify, id, map[string]string{"organization": org, "method": method}); err != nil {
		return nil, err
	}
	if s.o.Config.RequireApproval {
		_ = s.send(ctx, addr, awaitingMail(org))
		s.notifyOperators(ctx, row)
		return &Result{Status: StatusAwaitingApproval, OrganizationName: org, Username: addr}, nil
	}
	res, err := s.complete(ctx, row, by, Update{VerifiedAt: &now})
	if err != nil {
		// never left in creating (Expire doesn't take it up): an account that appeared meanwhile is existing (complete
		// may have said so already), any other failure expires the request; the person may sign up again
		st, why := StatusExpired, "the organization could not be created"
		if errors.Is(err, ErrConflict) {
			st, why = StatusExisting, "the email has an account"
		}
		_ = s.o.Store.SetStatus(ctx, id, Update{Status: st, Reason: why, ClearPassword: true})
		return nil, err
	}
	return res, nil
}

// Approve creates the organization of a signup awaiting approval (operators). The signup is claimed first, so two
// operators approving at once create one organization.
func (s *Service) Approve(ctx context.Context, by operator.Actor, id string) (*Result, error) {
	row, err := s.o.Store.Transition(ctx, id, StatusAwaitingApproval, StatusCreating)
	if errors.Is(err, ErrNotFound) {
		if _, gerr := s.o.Store.Get(ctx, id); gerr != nil {
			return nil, gerr
		}
		return nil, fmt.Errorf("%w: the signup is not awaiting approval", ErrConflict)
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	res, err := s.complete(ctx, row, signupActor(row.Email, row.RemoteIP), Update{VerifiedAt: row.VerifiedAt, DecidedAt: &now, DecidedBy: by.Username})
	if err != nil {
		if !errors.Is(err, ErrConflict) {
			_, _ = s.o.Store.Transition(ctx, id, StatusCreating, StatusAwaitingApproval)
		}
		return nil, err
	}
	if err := s.audit(ctx, by, AuditApplicationSignup, AuditActionApprove, id, map[string]string{"organization": res.Organization}); err != nil {
		s.o.Log.Errorf("signup %s: audit: %v", id, err)
	}
	_ = s.send(ctx, row.Email, readyMail(s.o.Config.PublicURL, row.OrgName, res.Organization, row.Method))
	return res, nil
}

// Deny refuses a signup awaiting approval (operators).
func (s *Service) Deny(ctx context.Context, by operator.Actor, id, reason string) error {
	row, err := s.o.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	if row.Status != StatusAwaitingApproval && row.Status != StatusPending {
		return fmt.Errorf("%w: the signup is %s", ErrConflict, row.Status)
	}
	now := s.now()
	if err := s.o.Store.SetStatus(ctx, id, Update{Status: StatusDenied, VerifiedAt: row.VerifiedAt, DecidedAt: &now, DecidedBy: by.Username, Reason: reason, ClearPassword: true}); err != nil {
		return err
	}
	if err := s.audit(ctx, by, AuditApplicationSignup, AuditActionDeny, id, map[string]string{"reason": reason}); err != nil {
		return err
	}
	if row.Status == StatusAwaitingApproval {
		_ = s.send(ctx, row.Email, deniedMail(row.OrgName, reason))
	}
	return nil
}

// Upkeep expires old signups and deletes the organizations whose deletion is due. One node runs it at a time.
func (s *Service) Upkeep(ctx context.Context) error {
	now := s.now()
	if _, err := s.o.Store.Expire(ctx, now); err != nil {
		return err
	}
	due, err := s.o.Store.DueDeletions(ctx, now)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range due {
		if err := s.runDeletion(ctx, d.OrganizationID, operator.Actor{Username: "signup:upkeep"}); err != nil && !errors.Is(err, ErrConflict) {
			errs = append(errs, fmt.Errorf("delete organization %s: %w", d.OrganizationID, err))
		}
	}
	return errors.Join(errs...)
}

// runDeletion deletes the organization of a due deletion. The deletion is claimed first (state deleting: no
// cancel and no reactivation from then on), then billing is told, the vault key shredded and the tenant deleted
// with everything it has. A claimed deletion that failed half way is taken up again by the next upkeep. ErrConflict
// when the deletion is off: the organization was reactivated meanwhile.
func (s *Service) runDeletion(ctx context.Context, org string, by operator.Actor) error {
	if _, err := s.o.Store.ClaimDeletion(ctx, org); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		return s.notClaimed(ctx, org, by)
	}
	if err := s.o.Plans.BeforeDelete(ctx, org); err != nil {
		return err
	}
	if s.o.Shredder != nil {
		if err := s.o.Shredder.Shred(ctx, tenant.ID(org)); err != nil {
			return err
		}
	}
	if err := s.o.Store.DeleteOrganization(ctx, org, by.Username, s.now()); err != nil {
		return err
	}
	s.o.Log.Infof("organization %s deleted", org)
	return s.audit(ctx, by, AuditApplicationDeletion, AuditActionDelete, org, nil)
}

// notClaimed handles a pending deletion that could not be claimed: its organization is gone (done), active again
// (canceled), or not due.
func (s *Service) notClaimed(ctx context.Context, org string, by operator.Actor) error {
	o, err := s.o.Orgs.Get(ctx, org)
	switch {
	case errors.Is(err, operator.ErrNotFound):
		if _, err := s.o.Store.FinishDeletion(ctx, org, DeletionDone, "gone", s.now()); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		return nil
	case err != nil:
		return err
	case o.Status != operator.StatusSuspended:
		if _, err := s.o.Store.FinishDeletion(ctx, org, DeletionCanceled, "reactivated", s.now()); err != nil {
			return err
		}
		if err := s.audit(ctx, by, AuditApplicationDeletion, AuditActionCancel, org, map[string]string{"reason": "the organization was reactivated"}); err != nil {
			return err
		}
		return fmt.Errorf("%w: organization %s was reactivated; its deletion is canceled", ErrConflict, org)
	default:
		return fmt.Errorf("%w: the deletion of organization %s is not due", ErrConflict, org)
	}
}

func ptr[T any](v T) *T { return &v }

// approvalExpiry: a verified signup expires a verified signup after approvalWait without a decision.
func approvalExpiry(now time.Time) time.Time { return now.Add(approvalWait) }
