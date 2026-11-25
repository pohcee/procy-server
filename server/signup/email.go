package signup

import (
	"context"
	"fmt"
	"strings"

	"github.com/openrport/openrport/server/operator"
)

// EmailStatus is whether the email of a signed-in account is confirmed.
type EmailStatus struct {
	Login    string `json:"login"`
	Verified bool   `json:"verified"`
	// Sent: a link to confirm it was mailed to the login.
	Sent bool `json:"sent"`
}

// RequestEmailConfirmation mails a link to the signed-in account login, when it is an email address, to prove the
// mailbox is its owner's (accounts an administrator created start unproven; they can't be invited into further
// organizations until confirmed).
func (s *Service) RequestEmailConfirmation(ctx context.Context, login, remoteIP string) (*EmailStatus, error) {
	addr, domain, err := normalizeEmail(login)
	if err != nil || addr != strings.ToLower(login) {
		return nil, fmt.Errorf("%w: the login is not an email address; it can't be confirmed", ErrInvalid)
	}
	acct, err := s.o.Accounts.Account(ctx, login)
	if err != nil {
		return nil, err
	}
	if !acct.Exists {
		return nil, ErrNotFound
	}
	if acct.Verified {
		return &EmailStatus{Login: login, Verified: true}, nil
	}
	now := s.now()
	n, err := s.o.Store.CountSince(ctx, now.Add(-day), remoteIP, domain, login)
	if err != nil {
		return nil, err
	}
	if over(n.Email, s.o.Config.MaxPerEmailPerDay) {
		return nil, ErrRateLimited
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	token, hash, err := newToken()
	if err != nil {
		return nil, err
	}
	// the login is kept as it is (not lower-cased): it names the account
	if err := s.o.Store.Insert(ctx, Signup{ID: id, Email: login, Domain: domain, RemoteIP: remoteIP, Status: StatusVerifyEmail,
		TokenHash: &hash, CreatedAt: now, ExpiresAt: now.Add(s.o.Config.TokenTTL)}); err != nil {
		return nil, err
	}
	if err := s.send(ctx, login, confirmEmailMail(s.o.Config.PublicURL, token, s.o.Config.TokenTTL)); err != nil {
		return nil, err
	}
	return &EmailStatus{Login: login, Sent: true}, nil
}

// ConfirmEmail takes the token of an email confirmation link (single use) and marks the account's email verified.
func (s *Service) ConfirmEmail(ctx context.Context, token, remoteIP string) (*EmailStatus, error) {
	if token == "" {
		return nil, ErrToken
	}
	now := s.now()
	row, err := s.o.Store.ClaimToken(ctx, hashToken(token), StatusVerifyEmail, now)
	if err != nil {
		return nil, err
	}
	if err := s.o.Accounts.MarkVerified(ctx, row.Email); err != nil {
		return nil, err
	}
	if err := s.o.Store.SetStatus(ctx, row.ID, Update{Status: StatusVerified, VerifiedAt: &now}); err != nil {
		s.o.Log.Errorf("email confirmation %s: %v", row.ID, err)
	}
	if err := s.audit(ctx, operator.Actor{Username: row.Email, RemoteIP: remoteIP}, AuditApplicationSignup, AuditActionVerifyEmail, row.Email, nil); err != nil {
		s.o.Log.Errorf("email confirmation %s: audit: %v", row.ID, err)
	}
	return &EmailStatus{Login: row.Email, Verified: true}, nil
}
