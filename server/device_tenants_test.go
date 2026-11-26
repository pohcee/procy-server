package chserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/server/clientsauth"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

// fakeDeviceTenants holds the tenant of each client auth id and the admission of each tenant; it records the
// admissions asked for.
type fakeDeviceTenants struct {
	ids        map[string]tenant.ID
	admissions map[tenant.ID]Admission
	admitErr   error
	asked      []string
}

func (f *fakeDeviceTenants) TenantOfClientAuth(_ context.Context, id string) (tenant.ID, bool, error) {
	tid, ok := f.ids[id]
	return tid, ok, nil
}

func (f *fakeDeviceTenants) Tenants(context.Context) ([]tenant.ID, error) {
	out := []tenant.ID{}
	for tid := range f.admissions {
		out = append(out, tid)
	}
	return out, nil
}

func (f *fakeDeviceTenants) AdmitClient(ctx context.Context, tid tenant.ID, clientID string) (Admission, error) {
	inCtx, _ := tenant.FromContext(ctx)
	f.asked = append(f.asked, fmt.Sprintf("%s/%s ctx=%s", tid, clientID, inCtx))
	if f.admitErr != nil {
		return Admission{}, f.admitErr
	}
	a, ok := f.admissions[tid]
	if !ok {
		return Admission{Status: TenantStatusActive}, nil
	}
	return a, nil
}

// tenantAuth is a client auth provider holding credentials per tenant; it records the tenant of each Get.
type tenantAuth struct {
	clientsauth.Provider
	creds map[tenant.ID]map[string]string
	gets  []tenant.ID
}

func (p *tenantAuth) Get(ctx context.Context, id string) (*clientsauth.ClientAuth, error) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	p.gets = append(p.gets, tid)
	if pass, ok := p.creds[tid][id]; ok {
		return &clientsauth.ClientAuth{ID: id, Password: pass}, nil
	}
	return nil, nil
}

type connMeta struct{ user string }

func (c connMeta) User() string          { return c.user }
func (c connMeta) SessionID() []byte     { return nil }
func (c connMeta) ClientVersion() []byte { return nil }
func (c connMeta) ServerVersion() []byte { return nil }
func (c connMeta) RemoteAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }
func (c connMeta) LocalAddr() net.Addr   { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2} }

func deviceTestListener(t *testing.T, dt DeviceTenants, auth clientsauth.Provider) *ClientListener {
	t.Helper()
	o := listenerTestOptions(t)
	o.DeviceTenants = dt
	o.ClientAuth = auth
	cl, err := NewClientListener(o)
	require.NoError(t, err)
	cl.ctx = tenant.WithID(context.Background(), tenant.Default)
	return cl
}

func twoTenantAuth() *tenantAuth {
	return &tenantAuth{creds: map[tenant.ID]map[string]string{
		tenant.Default: {"auth-d": "pass-d"},
		"a":            {"auth-a": "pass-a"},
		"b":            {"auth-b": "pass-b"},
	}}
}

func TestAuthUserFindsTheTenantOfTheClientAuthID(t *testing.T) {
	auth := twoTenantAuth()
	dt := &fakeDeviceTenants{ids: map[string]tenant.ID{"auth-a": "a", "auth-b": "b"}}
	cl := deviceTestListener(t, dt, auth)

	perms, err := cl.authUser(connMeta{"auth-b"}, []byte("pass-b"))
	require.NoError(t, err)
	require.NotNil(t, perms)
	assert.Equal(t, "b", perms.Extensions[permTenant])
	assert.Equal(t, []tenant.ID{"b"}, auth.gets, "the credentials are read in the tenant that holds the id")
	assert.Equal(t, tenant.ID("b"), deviceTenantOf(&ssh.ServerConn{Permissions: perms}))

	_, err = cl.authUser(connMeta{"auth-a"}, []byte("pass-b"))
	assert.Error(t, err, "wrong password")

	auth.gets = nil
	_, err = cl.authUser(connMeta{"auth-d"}, []byte("pass-d"))
	assert.Error(t, err, "an id no tenant holds is refused")
	assert.Empty(t, auth.gets, "and no tenant's credentials are read")
}

func TestAuthUserStandaloneUsesTheListenerTenant(t *testing.T) {
	auth := twoTenantAuth()
	cl := deviceTestListener(t, nil, auth)

	perms, err := cl.authUser(connMeta{"auth-d"}, []byte("pass-d"))
	require.NoError(t, err)
	assert.Nil(t, perms)
	assert.Equal(t, []tenant.ID{tenant.Default}, auth.gets)
	assert.Equal(t, tenant.ID(""), deviceTenantOf(&ssh.ServerConn{}))

	_, err = cl.authUser(connMeta{"auth-a"}, []byte("pass-a"))
	assert.Error(t, err, "another tenant's credentials are not found standalone")
}

func admissionLog() *logger.Logger {
	return logger.NewLogger("client", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)
}

func TestAdmitDevice(t *testing.T) {
	dt := &fakeDeviceTenants{admissions: map[tenant.ID]Admission{
		"full":      {Refused: RefusedLimit, Status: TenantStatusActive, Clients: 2, MaxClients: 1},
		"suspended": {Refused: RefusedStatus, Status: "suspended"},
		"gone":      {Refused: RefusedStatus, Status: "deleted"},
	}}
	cl := deviceTestListener(t, dt, twoTenantAuth())
	in := func(tid tenant.ID) context.Context { return tenant.WithID(context.Background(), tid) }
	// a client this process holds doesn't change the tenant's answer
	require.NoError(t, cl.getClientService().GetRepo().Save(in("full"), clients.New(t).ID("here").Logger(testLog).Build()))
	admit := func(tid tenant.ID, clientID string) error {
		return cl.admitDevice(in(tid), tid, clientID, "auth", admissionLog())
	}

	assert.NoError(t, admit("open", "c1"))
	assert.Equal(t, []string{"open/c1 ctx=open"}, dt.asked, "asked in the device's tenant")
	assert.EqualError(t, admit("full", "here"), "connection refused: the organization's limit of 1 clients is reached")
	assert.EqualError(t, admit("suspended", "c1"), "connection refused: the organization of this client is suspended")
	assert.EqualError(t, admit("gone", "c1"), "connection refused: the organization of this client is deleted")

	dt.admitErr = errors.New("database down")
	assert.EqualError(t, admit("open", "c1"), "connection refused: the server could not check the organization of this client; try again later")

	// a device without a tenant on a listener serving several is refused, never run in the listener's tenant
	dt.admitErr = nil
	dt.asked = nil
	assert.EqualError(t, admit("", "anything"), "connection refused: the organization of this client is unknown")
	assert.Empty(t, dt.asked)
}

// tenantStore is a ClientStore keeping clients per tenant; GetAll fails while failing is set.
type tenantStore struct {
	mu      sync.Mutex
	clients map[tenant.ID]map[string]*clientdata.Client
	failing bool
	saved   []string
}

func (s *tenantStore) GetAll(ctx context.Context, _ *logger.Logger) ([]*clientdata.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return nil, errors.New("database down")
	}
	tid, _ := tenant.FromContext(ctx)
	var out []*clientdata.Client
	for _, c := range s.clients[tid] {
		out = append(out, c)
	}
	return out, nil
}

func (s *tenantStore) Save(ctx context.Context, c *clientdata.Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tid, _ := tenant.FromContext(ctx)
	if s.clients[tid] == nil {
		s.clients[tid] = map[string]*clientdata.Client{}
	}
	s.clients[tid][c.GetID()] = c
	s.saved = append(s.saved, string(tid)+"/"+c.GetID())
	return nil
}

func (s *tenantStore) DeleteObsolete(context.Context, *logger.Logger) error { return nil }
func (s *tenantStore) Delete(context.Context, string, *logger.Logger) error { return nil }
func (s *tenantStore) Close() error                                         { return nil }

func storeListener(t *testing.T, dt DeviceTenants, store *tenantStore) *ClientListener {
	t.Helper()
	o := listenerTestOptions(t)
	o.DeviceTenants = dt
	o.ClientAuth = twoTenantAuth()
	o.ClientService = clients.NewClientService(nil, nil, clients.NewClientRepositoryWithDB(nil, nil, store, testLog), testLog, nil)
	cl, err := NewClientListener(o)
	require.NoError(t, err)
	cl.ctx = tenant.WithID(context.Background(), tenant.Default)
	return cl
}

// When the listener starts it loads every tenant's clients, so clients an earlier run left stored as connected are
// marked disconnected, not only the server's own tenant's.
func TestListenerStartLoadsEveryTenant(t *testing.T) {
	connected := clients.New(t).ID("b1").Logger(testLog).Build()
	require.True(t, connected.IsConnected())
	store := &tenantStore{clients: map[tenant.ID]map[string]*clientdata.Client{"b": {"b1": connected}}}
	dt := &fakeDeviceTenants{admissions: map[tenant.ID]Admission{"b": {Status: TenantStatusActive}}}
	cl := storeListener(t, dt, store)

	ctx, cancel := context.WithCancel(tenant.WithID(context.Background(), tenant.Default))
	defer cancel()
	require.NoError(t, cl.Start(ctx, freeAddr(t)))
	defer cl.Close()

	assert.Contains(t, store.saved, "b/b1")
	assert.False(t, store.clients["b"]["b1"].IsConnected(), "marked disconnected at start")
}

func TestAdmitDeviceWithoutDeviceTenants(t *testing.T) {
	cl := deviceTestListener(t, nil, twoTenantAuth())
	require.NoError(t, cl.admitDevice(tenant.WithID(context.Background(), tenant.Default), "", "c1", "auth", admissionLog()))
}
