package chserver

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// fakeDeviceTenants holds the tenant of each client auth id and the policy of each tenant.
type fakeDeviceTenants struct {
	ids       map[string]tenant.ID
	policies  map[tenant.ID]DevicePolicy
	policyErr error
}

func (f *fakeDeviceTenants) TenantOfClientAuth(ctx context.Context, id string) (tenant.ID, bool, error) {
	if _, err := tenant.FromContext(ctx); err != nil {
		return "", false, errors.New("the listener context carries the server's tenant")
	}
	tid, ok := f.ids[id]
	return tid, ok, nil
}

func (f *fakeDeviceTenants) DevicePolicy(_ context.Context, tid tenant.ID) (DevicePolicy, error) {
	if f.policyErr != nil {
		return DevicePolicy{}, f.policyErr
	}
	p, ok := f.policies[tid]
	if !ok {
		return DevicePolicy{Status: TenantStatusActive}, nil
	}
	return p, nil
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

func admissionLog() *logger.DynamicLogger {
	return logger.NewDynamicLogger("client", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug, false, false)
}

func TestAdmitDevice(t *testing.T) {
	dt := &fakeDeviceTenants{policies: map[tenant.ID]DevicePolicy{
		"limited":   {Status: TenantStatusActive, Limited: true, MaxClients: 1},
		"none":      {Status: TenantStatusActive, Limited: true, MaxClients: 0},
		"suspended": {Status: "suspended"},
	}}
	cl := deviceTestListener(t, dt, twoTenantAuth())
	repo := cl.getClientService().GetRepo()
	in := func(tid tenant.ID) context.Context { return tenant.WithID(context.Background(), tid) }
	require.NoError(t, repo.Save(in("limited"), clients.New(t).ID("known").Logger(testLog).Build()))
	require.NoError(t, repo.Save(in("open"), clients.New(t).ID("other").Logger(testLog).Build()))

	admit := func(tid tenant.ID, clientID string) error {
		release, err := cl.admitDevice(in(tid), tid, clientID, "auth", admissionLog())
		require.NotNil(t, release)
		release()
		return err
	}

	assert.NoError(t, admit("limited", "known"), "a client the tenant has is never refused")
	err := admit("limited", "new")
	assert.EqualError(t, err, "connection refused: the organization's limit of 1 clients is reached")
	assert.Error(t, admit("none", "new"), "a limit of 0 takes no client")
	assert.NoError(t, admit("open", "new"), "no limit")
	assert.NoError(t, admit("open", "new2"))
	assert.EqualError(t, admit("suspended", "known"), "connection refused: the organization of this client is suspended")

	// deleting a client frees its place
	known, err := repo.GetByID(in("limited"), "known")
	require.NoError(t, err)
	require.NoError(t, repo.Delete(in("limited"), known))
	assert.NoError(t, admit("limited", "new"))

	dt.policyErr = errors.New("database down")
	assert.ErrorContains(t, admit("open", "new"), "could not check the organization")

	// standalone (no tenant found by authUser): always admitted
	assert.NoError(t, admit("", "anything"))
}

func TestAdmitDeviceWithoutDeviceTenants(t *testing.T) {
	cl := deviceTestListener(t, nil, twoTenantAuth())
	release, err := cl.admitDevice(tenant.WithID(context.Background(), tenant.Default), "", "c1", "auth", admissionLog())
	require.NoError(t, err)
	release()
}
