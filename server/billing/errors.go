package billing

import (
	"errors"
	"math"
)

// Errors of the payment provider integration.
var (
	// ErrNotFound: no such organization, record or provider object.
	ErrNotFound = errors.New("billing: not found")
	// ErrConflict: the request doesn't fit the organization's billing state (an exempt organization asking for
	// checkout, a suspension that isn't proposed, a plan without provider prices).
	ErrConflict = errors.New("billing: conflict")
	// ErrReadOnly is the error of a refused change while an organization is read-only for an overdue payment.
	ErrReadOnly = errors.New("billing: payment overdue: the organization is read-only until the payment succeeds")
)

// GiBRoundedUp is bytes in whole GiB, rounded up (the overage unit).
func GiBRoundedUp(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(bytes) / float64(GiB)))
}
