package signup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openrport/openrport/server/operator"
)

// maxReason bounds the reason given for a deletion or a denial.
const maxReason = 1000

func checkOrganization(org string) error {
	if org == "" || strings.HasPrefix(org, "_") {
		return ErrNotFound
	}
	return nil
}

// RequestDeletion starts the deletion of an organization: it is suspended at once (nobody can log in, devices
// can't connect), its billing is closed, and its data is deleted after [signup] deletion_hold. by is the
// organization's administrator (byOperator false: the administrators get a link to cancel, since they can't log in
// to the suspended organization) or an operator (byOperator true: they get a notice, and only an operator can
// cancel).
func (s *Service) RequestDeletion(ctx context.Context, by operator.Actor, org, reason string, byOperator bool) (*Deletion, error) {
	if err := checkOrganization(org); err != nil {
		return nil, err
	}
	if len(reason) > maxReason {
		return nil, fmt.Errorf("%w: the reason must be at most %d characters", ErrInvalid, maxReason)
	}
	o, err := s.o.Orgs.Get(ctx, org)
	if errors.Is(err, operator.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// the administrators, read before the suspension, are told
	admins, err := s.o.Store.AdminEmails(ctx, org)
	if err != nil {
		return nil, err
	}
	if !byOperator && len(admins) == 0 {
		return nil, fmt.Errorf("%w: no administrator of the organization has an email address for the link to cancel the deletion", ErrConflict)
	}
	var token string
	var hash *string
	if !byOperator {
		t, h, err := newToken()
		if err != nil {
			return nil, err
		}
		token, hash = t, &h
	}
	now := s.now()
	d, err := s.o.Store.AddDeletion(ctx, Deletion{
		OrganizationID: org, OrganizationName: o.Name, State: DeletionPending, RequestedAt: now, RequestedBy: by.Username,
		Reason: reason, DeleteAfter: now.Add(s.o.Config.DeletionHold), CancelTokenHash: hash, PreviousStatus: o.Status,
	})
	if err != nil {
		return nil, err
	}
	undo := func() { _, _ = s.o.Store.FinishDeletion(ctx, org, DeletionCanceled, "signup:request-failed", now) }
	if err := s.audit(ctx, by, AuditApplicationDeletion, AuditActionRequest, org, map[string]any{"reason": reason, "delete_after": d.DeleteAfter}); err != nil {
		undo()
		return nil, err
	}
	if o.Status != operator.StatusSuspended {
		if _, err := s.o.Orgs.SetStatus(ctx, by, org, operator.StatusSuspended); err != nil {
			undo()
			return nil, err
		}
	}
	if err := s.o.Plans.DeletionRequested(ctx, by, org); err != nil {
		s.o.Log.Errorf("deletion of organization %s: billing: %v", org, err)
	}
	for _, a := range admins {
		m := deletionNotice(o.Name, org, d.DeleteAfter)
		if token != "" {
			m = deletionMail(s.o.Config.PublicURL, o.Name, org, token, d.DeleteAfter)
		}
		if s.send(ctx, a, m) == nil && token != "" {
			d.CancelLinksSent++
		}
	}
	return d, nil
}

// CancelDeletion ends the pending deletion of an organization (operators) and gives it back the status it had.
func (s *Service) CancelDeletion(ctx context.Context, by operator.Actor, org string) (*Deletion, error) {
	if err := checkOrganization(org); err != nil {
		return nil, err
	}
	d, err := s.o.Store.FinishDeletion(ctx, org, DeletionCanceled, by.Username, s.now())
	if err != nil {
		return nil, err
	}
	if d.PreviousStatus == operator.StatusActive {
		if _, err := s.o.Orgs.SetStatus(ctx, by, org, operator.StatusActive); err != nil {
			return nil, err
		}
	}
	if err := s.audit(ctx, by, AuditApplicationDeletion, AuditActionCancel, org, nil); err != nil {
		return nil, err
	}
	return d, nil
}

// CancelDeletionByToken cancels a deletion with the link mailed to the organization's administrators.
func (s *Service) CancelDeletionByToken(ctx context.Context, token, remoteIP string) (*Deletion, error) {
	if token == "" {
		return nil, ErrToken
	}
	d, err := s.o.Store.DeletionByCancelToken(ctx, hashToken(token))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrToken
	}
	if err != nil {
		return nil, err
	}
	return s.CancelDeletion(ctx, operator.Actor{Username: "signup:cancel-link", RemoteIP: remoteIP}, d.OrganizationID)
}

// Expedite deletes an organization whose deletion is pending now, without waiting for its date (operators).
// ErrConflict when the organization was reactivated meanwhile (the deletion is then canceled).
func (s *Service) Expedite(ctx context.Context, by operator.Actor, org string) error {
	if err := checkOrganization(org); err != nil {
		return err
	}
	if _, err := s.o.Store.PendingDeletion(ctx, org); err != nil {
		return err
	}
	if err := s.o.Store.SetDeleteAfter(ctx, org, time.Time{}); err != nil { // now, by the database clock
		return err
	}
	if err := s.audit(ctx, by, AuditApplicationDeletion, AuditActionExpedite, org, nil); err != nil {
		return err
	}
	return s.runDeletion(ctx, org, by)
}

// ListDeletions returns the newest deletions first; state "" is every state (operators).
func (s *Service) ListDeletions(ctx context.Context, state string, limit int) ([]Deletion, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.o.Store.ListDeletions(ctx, state, limit)
}

// Export returns the configuration of an organization without secrets, and audits who took it.
func (s *Service) Export(ctx context.Context, by operator.Actor, org string) (*Export, error) {
	if err := checkOrganization(org); err != nil {
		return nil, err
	}
	e, err := s.o.Store.Export(ctx, org)
	if err != nil {
		return nil, err
	}
	if err := s.audit(ctx, by, AuditApplicationExport, AuditActionExport, org, nil); err != nil {
		return nil, err
	}
	return e, nil
}
