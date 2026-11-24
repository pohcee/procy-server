package chserver

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/openrport/openrport/db/migration/api_sessions"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/bearer"
	chshare "github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

var testLog = chshare.NewLogger("chserver-test", chshare.LogOutput{File: os.Stdout}, chshare.LogLevelDebug)
var hour = time.Hour

var DataSourceOptions = sqlite.DataSourceOptions{WALEnabled: false}

type mockConnection struct {
	ssh.Conn
	closed bool
}

func (m *mockConnection) Close() error {
	m.closed = true
	return nil
}

func newEmptyAPISessionCache(t *testing.T) *session.Cache {
	storage, err := openTestSessionProvider()
	require.NoError(t, err)
	c, err := session.NewCache(tenant.WithID(context.Background(), tenant.Default), bearer.DefaultTokenLifetime, cleanupAPISessionsInterval, storage, nil)
	require.NoError(t, err)
	return c
}

func openTestSessionProvider() (*session.SqliteProvider, error) {
	db, err := sqlite.New(":memory:", api_sessions.FS, DataSourceOptions)
	if err != nil {
		return nil, err
	}
	return session.NewSqliteProvider(db), nil
}
