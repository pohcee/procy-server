package clients

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/clients"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/tenant"
)

var DataSourceOptions = sqlite.DataSourceOptions{WALEnabled: false}

func NewFakeClientProvider(t *testing.T, exp *time.Duration, cs ...*clientdata.Client) *SqliteProvider {
	db, err := sqlite.New(":memory:", clients.FS, DataSourceOptions)
	require.NoError(t, err)
	p := newSqliteProvider(db, exp)
	for _, cur := range cs {
		if cur != nil {
			err = p.Save(tenant.WithID(context.Background(), tenant.Default), cur)
			require.NoError(t, err)
		}
	}
	return p
}
