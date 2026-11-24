package auditlog

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/auditlog"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/tenant"
)

var DataSourceOptions = sqlite.DataSourceOptions{WALEnabled: false}

func TestNotEnabled(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)

	mockProvider := &mockProvider{}
	auditLog, err := New(context.Background(), nil, nil, config.Config{Enable: false}, nil)
	require.NoError(t, err)
	auditLog.provider = mockProvider

	// Call with all methods to make sure it doesn't panic if not initialized
	e := auditLog.Entry(ApplicationAuthUser, ActionCreate).
		WithID(123).
		WithHTTPRequest(req).
		WithRequest(map[string]interface{}{}).
		WithResponse(map[string]interface{}{}).
		WithClient(&clientdata.Client{}).
		WithClientID("123")

	e.Save()
	e.SaveForMultipleClients([]*clientdata.Client{&clientdata.Client{}})

	assert.Equal(t, 0, len(mockProvider.entries))
}

func TestIPObfuscation(t *testing.T) {
	testCases := []struct {
		RemoteIP            string
		ExpectedObfuscation string
	}{
		{
			RemoteIP:            "192.0.2.123",
			ExpectedObfuscation: "192.0.2.x",
		}, {
			RemoteIP:            "2001:0db8:85a3:0000:0000:8a2e:0370:7334",
			ExpectedObfuscation: "2001:0db8:85a3:0000:0000:8a2e:0370:7334",
		}, {
			RemoteIP:            "example.com",
			ExpectedObfuscation: "example.com",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.RemoteIP, func(t *testing.T) {
			t.Parallel()

			t.Run("with obfuscation", func(t *testing.T) {
				mockProvider := &mockProvider{}
				auditLog := &AuditLog{
					config: config.Config{
						Enable:           true,
						UseIPObfuscation: true,
					},
					provider: mockProvider,
				}

				e := auditLog.Entry("", "")
				e.RemoteIP = tc.RemoteIP
				e.Save()

				assert.Equal(t, tc.ExpectedObfuscation, mockProvider.entries[0].RemoteIP)
			})

			t.Run("without obfuscation", func(t *testing.T) {
				mockProvider := &mockProvider{}
				auditLog := &AuditLog{
					config: config.Config{
						Enable:           true,
						UseIPObfuscation: false,
					},
					provider: mockProvider,
				}

				e := auditLog.Entry("", "")
				e.RemoteIP = tc.RemoteIP
				e.Save()

				assert.Equal(t, tc.RemoteIP, mockProvider.entries[0].RemoteIP)
			})
		})
	}
}

func TestList(t *testing.T) {
	var tests = []struct {
		desc              string
		user              users.User
		filter            string
		expectedError     string
		expectedResultLen int
	}{
		{
			"Admin success",
			users.User{Username: "Admin", Groups: []string{"Administrators"}},
			"filter[application]=library.script&sort=-timestamp&page[limit]=1&page[offset]=1",
			"",
			1,
		},
		{
			"No Admin denied",
			users.User{Username: "Loser", Groups: []string{"Losers"}},
			"filter[username]=Admin",
			"only members of group Administrators can filter by usernames",
			0,
		},
		{
			"No Admin No results",
			users.User{Username: "Loser", Groups: []string{"Losers"}},
			"",
			"",
			0,
		},
	}
	db, err := sqlite.New(":memory:", auditlog.FS, DataSourceOptions)
	require.NoError(t, err)
	dbProv := &SQLiteProvider{
		db: db,
	}
	auditLog := &AuditLog{
		config: config.Config{
			Enable: true,
		},
		provider: dbProv,
	}
	defer auditLog.Close()

	ctx := ctxFor(tenant.Default)
	auditLog.Entry(ApplicationLibraryScript, ActionCreate).WithContext(ctx).Save()
	auditLog.Entry(ApplicationLibraryScript, ActionUpdate).WithContext(ctx).Save()
	auditLog.Entry(ApplicationLibraryCommand, ActionCreate).WithContext(ctx).Save()

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/auditlog?"+tc.filter, nil).WithContext(ctx)
			result, err := auditLog.List(r, &tc.user)
			if tc.expectedError != "" {
				assert.Error(t, err, tc.expectedError)
			} else {
				require.NoError(t, err)
				entries := result.Data.([]*Entry)
				assert.Equal(t, tc.expectedResultLen, len(entries))
				if tc.expectedResultLen > 0 {
					assert.Equal(t, 2, result.Meta.Count)
					assert.Equal(t, ApplicationLibraryScript, entries[0].Application)
					assert.Equal(t, ActionCreate, entries[0].Action)
				}
			}
		})
	}

}

func TestNewWithProvider(t *testing.T) {
	p := &mockProvider{}
	auditLog, err := New(context.Background(), nil, nil, config.Config{Enable: true}, p)
	require.NoError(t, err)

	auditLog.Entry(ApplicationLibraryScript, ActionCreate).WithContext(ctxFor(tenant.Default)).Save()

	require.Len(t, p.entries, 1)
	assert.Equal(t, ApplicationLibraryScript, p.entries[0].Application)
}

func TestNewEnabledWithoutProvider(t *testing.T) {
	_, err := New(context.Background(), nil, nil, config.Config{Enable: true}, nil)
	assert.Error(t, err)
}
