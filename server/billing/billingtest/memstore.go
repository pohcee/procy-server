// Package billingtest has test doubles of billing.
package billingtest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/openrport/openrport/server/billing"
)

var _ billing.Store = (*MemStore)(nil)

// MemStore is an in-memory billing.Store for tests.
type MemStore struct {
	mu sync.Mutex
	// upd serializes the read-change-write of accounts, as the Postgres store's lock does
	upd sync.Mutex
	// Accts by organization, Rpts by identifier, Emails of each organization's administrators; Events,
	// Notices and Audits record what was claimed and audited.
	Accts   map[string]billing.ProviderAccount
	Events  map[string]bool
	Rpts    map[string]billing.UsageReport
	Notices map[string]bool
	Emails  map[string][]string
	Audits  []string
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{Accts: map[string]billing.ProviderAccount{}, Events: map[string]bool{}, Rpts: map[string]billing.UsageReport{},
		Notices: map[string]bool{}, Emails: map[string][]string{}}
}

func (m *MemStore) Account(_ context.Context, org string) (*billing.ProviderAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.Accts[org]
	if !ok {
		return nil, nil
	}
	return &a, nil
}

func (m *MemStore) Accounts(context.Context) ([]billing.ProviderAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []billing.ProviderAccount
	for _, a := range m.Accts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Organization < out[j].Organization })
	return out, nil
}

func (m *MemStore) OrganizationOf(_ context.Context, provider, customer string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.Accts {
		if a.Provider == provider && a.CustomerID == customer {
			return a.Organization, nil
		}
	}
	return "", nil
}

func (m *MemStore) PutAccount(_ context.Context, a *billing.ProviderAccount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Accts[a.Organization] = *a
	return nil
}

// UpdateAccount implements billing.Store.
func (m *MemStore) UpdateAccount(ctx context.Context, org string, update func(*billing.ProviderAccount) (*billing.ProviderAccount, error)) error {
	m.upd.Lock()
	defer m.upd.Unlock()
	cur, _ := m.Account(ctx, org)
	next, err := update(cur)
	if err != nil || next == nil {
		return err
	}
	return m.PutAccount(ctx, next)
}

func (m *MemStore) ApplyEvent(ctx context.Context, provider, id, _, org string, update func(*billing.ProviderAccount) (*billing.ProviderAccount, error)) (bool, error) {
	m.upd.Lock()
	defer m.upd.Unlock()
	m.mu.Lock()
	if m.Events[provider+"/"+id] {
		m.mu.Unlock()
		return false, nil
	}
	m.mu.Unlock()
	var cur *billing.ProviderAccount
	if org != "" {
		cur, _ = m.Account(ctx, org)
	}
	next, err := update(cur)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	m.Events[provider+"/"+id] = true
	m.mu.Unlock()
	if next != nil {
		return true, m.PutAccount(ctx, next)
	}
	return true, nil
}

func (m *MemStore) Reports(_ context.Context, start time.Time) ([]billing.UsageReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []billing.UsageReport
	for _, r := range m.Rpts {
		if start.IsZero() || r.PeriodStart.Equal(start) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identifier < out[j].Identifier })
	return out, nil
}

func (m *MemStore) PutReport(_ context.Context, r *billing.UsageReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.Rpts[r.Identifier]; ok && old.Quantity != r.Quantity {
		return errors.New("quantity changed")
	}
	m.Rpts[r.Identifier] = *r
	return nil
}

func (m *MemStore) ClaimNotice(_ context.Context, org, kind, ep string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := org + "/" + kind + "/" + ep
	if m.Notices[k] {
		return false, nil
	}
	m.Notices[k] = true
	return true, nil
}

func (m *MemStore) Audit(_ context.Context, by billing.Actor, action, org string, _ any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Audits = append(m.Audits, by.Username+" "+action+" "+org)
	return nil
}

func (m *MemStore) AdminEmails(_ context.Context, org string) ([]string, error) {
	return m.Emails[org], nil
}
