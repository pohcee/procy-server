package clients

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/migration/clients"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/tenant"
)

var DataSourceOptions = sqlite.DataSourceOptions{WALEnabled: false}

func NewFakeClientProvider(t *testing.T, exp *time.Duration, cs ...*clientdata.Client) *SqliteProvider {
	db, err := sqlite.New(":memory:", clients.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSQLiteStore(db, exp)
	for _, cur := range cs {
		if cur != nil {
			err = p.Save(tenant.WithID(context.Background(), tenant.Default), cur)
			require.NoError(t, err)
		}
	}
	return p
}
