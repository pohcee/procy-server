package billing

import (
	"fmt"
	"time"
)

// DunningState is where an organization stands after a failed payment (billing and metering design, Failed payment
// and dunning). The provider retries the card and sends its own emails; rportd follows the state from webhooks and
// the dunning tick.
type DunningState string

const (
	// DunningNone: nothing is overdue.
	DunningNone DunningState = "none"
	// DunningPastDue: a payment failed; full service during the grace period.
	DunningPastDue DunningState = "past_due"
	// DunningReadOnly: the grace period passed; users can log in and see everything, nothing changes. Devices stay
	// connected and existing tunnels stay up.
	DunningReadOnly DunningState = "read_only"
	// DunningSuspensionProposed: read-only for its whole period; operators are asked to confirm the suspension.
	// Still read-only until then.
	DunningSuspensionProposed DunningState = "suspension_proposed"
)

// Access is what an organization's users may do, from its dunning state.
type Access string

const (
	// AccessFull: everything.
	AccessFull Access = "full"
	// AccessReadOnly: no new jobs, tunnels, uploads or configuration changes; schedules pause.
	AccessReadOnly Access = "read_only"
)

// Default dunning periods ([billing] grace_days, read_only_days).
const (
	DefaultGraceDays    = 14
	DefaultReadOnlyDays = 14
)

// Dunning is an organization's dunning record.
type Dunning struct {
	State DunningState `json:"state" db:"dunning_state"`
	// FailedAt is when the first payment of this episode failed.
	FailedAt *time.Time `json:"failed_at" db:"payment_failed_at"`
	// ReadOnlyAt and ProposedAt are when the organization entered those states.
	ReadOnlyAt *time.Time `json:"read_only_at" db:"read_only_at"`
	ProposedAt *time.Time `json:"suspension_proposed_at" db:"suspension_proposed_at"`
	// FailedInvoice is the invoice whose payment failed first.
	FailedInvoice string `json:"failed_invoice,omitempty" db:"failed_invoice"`
}

// Access is the access that follows from the state.
func (d Dunning) Access() Access {
	switch d.State {
	case DunningReadOnly, DunningSuspensionProposed:
		return AccessReadOnly
	}
	return AccessFull
}

// DunningPolicy is how long each state lasts.
type DunningPolicy struct {
	Grace    time.Duration
	ReadOnly time.Duration
}

// DefaultDunningPolicy is 14 days of grace, then 14 days read-only.
var DefaultDunningPolicy = DunningPolicy{Grace: DefaultGraceDays * 24 * time.Hour, ReadOnly: DefaultReadOnlyDays * 24 * time.Hour}

// PaymentFailed is the state after a failed payment of invoice at at. A failure while already overdue changes
// nothing: the episode keeps its first failure, so retries don't restart the grace period.
func (d Dunning) PaymentFailed(at time.Time, invoice string) Dunning {
	if d.State != "" && d.State != DunningNone {
		return d
	}
	at = at.UTC()
	return Dunning{State: DunningPastDue, FailedAt: &at, FailedInvoice: invoice}
}

// PaymentSucceeded ends the episode: everything is restored at once.
func (d Dunning) PaymentSucceeded() Dunning { return Dunning{State: DunningNone} }

// Tick moves the state on with time: past_due → read_only after the grace period, read_only →
// suspension_proposed after the read-only period (both counted from the first failure). A state that skipped
// ahead (the tick didn't run for a while) passes through read_only first, so read-only is always notified.
func (d Dunning) Tick(now time.Time, p DunningPolicy) Dunning {
	if d.FailedAt == nil {
		return d
	}
	now = now.UTC()
	switch d.State {
	case DunningPastDue:
		if !now.Before(d.FailedAt.Add(p.Grace)) {
			d.State = DunningReadOnly
			d.ReadOnlyAt = &now
		}
	case DunningReadOnly:
		if !now.Before(d.FailedAt.Add(p.Grace + p.ReadOnly)) {
			d.State = DunningSuspensionProposed
			d.ProposedAt = &now
		}
	}
	return d
}

// Restore is an operator moving an organization out of read-only (or past due): the episode ends without a
// payment. The next failed payment starts a new one.
func (d Dunning) Restore() Dunning { return Dunning{State: DunningNone} }

// CanConfirmSuspension reports whether operators may confirm the suspension now.
func (d Dunning) CanConfirmSuspension() error {
	if d.State != DunningSuspensionProposed {
		return fmt.Errorf("%w: suspension is not proposed (dunning state %q)", ErrConflict, d.state())
	}
	return nil
}

func (d Dunning) state() DunningState {
	if d.State == "" {
		return DunningNone
	}
	return d.State
}
