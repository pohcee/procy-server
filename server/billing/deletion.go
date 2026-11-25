package billing

import (
	"context"
	"fmt"
)

// CancelForDeletion cancels the organization's subscription at once, because its deletion was requested
// (self-service signup's organization deletion). The provider's final invoice covers the partial period; an
// organization without a subscription has nothing to cancel.
func (s *Service) CancelForDeletion(ctx context.Context, org string) error {
	b, err := s.record(ctx, org)
	if err != nil {
		return err
	}
	if b.ProviderSubscriptionID == "" {
		return nil
	}
	if err := s.o.Provider.Cancel(ctx, b.ProviderSubscriptionID, false); err != nil {
		return fmt.Errorf("billing: cancel the subscription of %s: %w", org, err)
	}
	return nil
}
