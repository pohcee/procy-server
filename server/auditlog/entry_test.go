package auditlog

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/auditlog/config"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

func TestEntry(t *testing.T) {
	auditLog := enabledAuditLog()
	e := auditLog.Entry(ApplicationClient, ActionCreate)

	assert.WithinDuration(t, time.Now(), e.Timestamp, time.Millisecond)
	assert.Equal(t, ApplicationClient, e.Application)
	assert.Equal(t, ActionCreate, e.Action)
}

func TestWithID(t *testing.T) {
	testCases := []struct {
		Name   string
		ID     interface{}
		wantID string
	}{
		{
			Name:   "string",
			ID:     "abc",
			wantID: "abc",
		}, {
			Name:   "uuid",
			ID:     uuid.MustParse("11236310-6cad-408e-b372-a0f04d68d2df"),
			wantID: "11236310-6cad-408e-b372-a0f04d68d2df",
		}, {
			Name:   "number",
			ID:     123,
			wantID: "123",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			e := emptyEntry().WithID(tc.ID)

			assert.Equal(t, tc.wantID, e.ID)
		})
	}
}

func TestWithHTTPRequest(t *testing.T) {
	ctx := api.WithUser(context.Background(), "test-user")
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(ctx)

	e := emptyEntry().WithHTTPRequest(req)

	assert.Equal(t, "test-user", e.Username)
	assert.Equal(t, "192.0.2.1", e.RemoteIP)
}

func TestWithRequest(t *testing.T) {
	e := emptyEntry().WithRequest(map[string]interface{}{
		"k1": "v1",
		"k2": 2,
	})

	assert.JSONEq(t, `{"k1": "v1", "k2": 2}`, e.Request)
}

func TestWithResponse(t *testing.T) {
	e := emptyEntry().WithResponse(map[string]interface{}{
		"k1": "v1",
		"k2": 2,
	})

	assert.JSONEq(t, `{"k1": "v1", "k2": 2}`, e.Response)
}

func TestWithClient(t *testing.T) {
	c1 := clientdata.Client{}
	c1.SetID("11236310-6cad-408e-b372-a0f04d68d2df")
	c1.SetAddress("127.0.0.1")
	c1.SetHostname("hostname")

	e := emptyEntry().WithClient(&c1)

	assert.Equal(t, "11236310-6cad-408e-b372-a0f04d68d2df", e.ClientID)
	assert.Equal(t, "hostname", e.ClientHostName)
}

func TestWithClientID(t *testing.T) {
	auditLog := enabledAuditLog()
	auditLog.clientGetter = &mockClientGetter{}

	ctx := tenant.WithID(context.Background(), tenant.Default)

	t.Run("client exists", func(t *testing.T) {
		e := auditLog.Entry("", "").WithContext(ctx).WithClientID("11236310-6cad-408e-b372-a0f04d68d2df")
		assert.Equal(t, "11236310-6cad-408e-b372-a0f04d68d2df", e.ClientID)
		assert.Equal(t, "hostname", e.ClientHostName)
	})

	t.Run("client does not exist", func(t *testing.T) {
		e := auditLog.Entry("", "").WithContext(ctx).WithClientID("2e7bf232-b4aa-4cdb-a7bb-d28f63712c2d")
		assert.Equal(t, "2e7bf232-b4aa-4cdb-a7bb-d28f63712c2d", e.ClientID)
		assert.Equal(t, "", e.ClientHostName)
	})
}

func TestWithClientIDLooksUpClientInEntryTenant(t *testing.T) {
	auditLog := enabledAuditLog()
	auditLog.logger = logger.NewLogger("auditlog-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)
	getter := &mockClientGetter{}
	auditLog.clientGetter = getter
	const cid = "11236310-6cad-408e-b372-a0f04d68d2df"

	t.Run("tenant from the http request", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req = req.WithContext(tenant.WithID(api.WithUser(req.Context(), "admin"), "a"))
		e := auditLog.Entry("", "").WithHTTPRequest(req).WithClientID(cid)
		assert.Equal(t, tenant.ID("a"), getter.gotTenant)
		assert.Equal(t, "hostname", e.ClientHostName)
	})

	t.Run("tenant from WithContext", func(t *testing.T) {
		e := auditLog.Entry("", "").WithContext(tenant.WithID(context.Background(), "b")).WithClientID(cid)
		assert.Equal(t, tenant.ID("b"), getter.gotTenant)
		assert.Equal(t, "hostname", e.ClientHostName)
	})

	t.Run("no tenant", func(t *testing.T) {
		e := auditLog.Entry("", "").WithClientID(cid)
		assert.Equal(t, cid, e.ClientID)
		assert.Equal(t, "", e.ClientHostName)
	})
}

func TestSave(t *testing.T) {
	mockProvider := &mockProvider{}
	auditLog := enabledAuditLog()

	t.Run("with provider", func(t *testing.T) {
		auditLog.provider = mockProvider

		e := auditLog.Entry(ApplicationClient, ActionCreate)
		e.Save()

		assert.Equal(t, []Entry{*e}, mockProvider.entries)
	})

	t.Run("with nil provider", func(_ *testing.T) {
		auditLog.provider = nil

		auditLog.Entry("", "").Save()
	})
}

func TestSaveForMultipleClients(t *testing.T) {
	mockProvider := &mockProvider{}
	auditLog := enabledAuditLog()
	auditLog.provider = mockProvider

	c1 := clientdata.Client{}
	c1.SetID("c1")
	c1.SetAddress("c1.com")
	c1.SetHostname("hostname1")

	c2 := clientdata.Client{}
	c2.SetID("c2")
	c2.SetAddress("c2.com")
	c2.SetHostname("hostname2")

	auditLog.Entry("", "").SaveForMultipleClients([]*clientdata.Client{&c1, &c2})

	assert.Len(t, mockProvider.entries, 2)
	assert.Equal(t, "c1", mockProvider.entries[0].ClientID)
	assert.Equal(t, "hostname1", mockProvider.entries[0].ClientHostName)
	assert.Equal(t, "c2", mockProvider.entries[1].ClientID)
	assert.Equal(t, "hostname2", mockProvider.entries[1].ClientHostName)
}

func enabledAuditLog() *AuditLog {
	return &AuditLog{
		config: config.Config{
			Enable: true,
		},
	}
}

func emptyEntry() *Entry {
	return enabledAuditLog().Entry("", "")
}

type mockClientGetter struct {
	gotTenant tenant.ID
}

func (m *mockClientGetter) GetByID(ctx context.Context, id string) (*clientdata.Client, error) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	m.gotTenant = tid

	c1 := clientdata.Client{}
	c1.SetID("11236310-6cad-408e-b372-a0f04d68d2df")
	c1.SetAddress("127.0.0.1")
	c1.SetHostname("hostname")

	if id == "11236310-6cad-408e-b372-a0f04d68d2df" {
		return &c1, nil
	}
	return nil, nil
}

type mockProvider struct {
	entries []Entry
}

func (p *mockProvider) Save(_ context.Context, e *Entry) error {
	p.entries = append(p.entries, *e)
	return nil
}

func (p *mockProvider) List(_ context.Context, _ *query.ListOptions) ([]*Entry, error) {
	return nil, nil
}
func (p *mockProvider) Count(_ context.Context, _ *query.ListOptions) (int, error) {
	return 0, nil
}
func (p mockProvider) Close() error { return nil }
