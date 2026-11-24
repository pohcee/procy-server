package jobs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/jobs"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/test/jb"
	"github.com/openrport/openrport/share/tenant"
)

func TestCleanupJobsMultiJobs(t *testing.T) {
	ctx := ctxFor(tenant.Default)
	jobsDB, err := sqlite.New(":memory:", jobs.AssetNames(), jobs.Asset, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(jobsDB, testLog)
	defer p.Close()

	mj1 := jb.NewMulti(t).Build()
	mj2 := jb.NewMulti(t).Build()
	mj3 := jb.NewMulti(t).Build()
	require.NoError(t, p.SaveMultiJob(ctx, mj1))
	require.NoError(t, p.SaveMultiJob(ctx, mj2))
	require.NoError(t, p.SaveMultiJob(ctx, mj3))

	j1 := jb.New(t).MultiJobID(mj1.JID).StartedAt(time.Now()).Build()
	j2 := jb.New(t).MultiJobID(mj1.JID).StartedAt(time.Now()).Build()
	j3 := jb.New(t).StartedAt(time.Now()).Build()
	j4 := jb.New(t).MultiJobID(mj2.JID).StartedAt(time.Now()).Build()
	j5 := jb.New(t).StartedAt(time.Now()).Build()
	j6 := jb.New(t).MultiJobID(mj2.JID).StartedAt(time.Now()).Build()
	j7 := jb.New(t).MultiJobID(mj3.JID).StartedAt(time.Now()).Build()
	j8 := jb.New(t).MultiJobID(mj3.JID).StartedAt(time.Now()).Build()
	require.NoError(t, p.SaveJob(ctx, j1))
	require.NoError(t, p.SaveJob(ctx, j2))
	require.NoError(t, p.SaveJob(ctx, j3))
	require.NoError(t, p.SaveJob(ctx, j4))
	require.NoError(t, p.SaveJob(ctx, j5))
	require.NoError(t, p.SaveJob(ctx, j6))
	require.NoError(t, p.SaveJob(ctx, j7))
	require.NoError(t, p.SaveJob(ctx, j8))

	err = p.CleanupJobsMultiJobs(ctx, 3)
	require.NoError(t, err)

	// mj1 and mj2 should be deleted
	mj, err := p.GetMultiJob(ctx, mj1.JID)
	require.NoError(t, err)
	assert.Nil(t, mj)
	mj, err = p.GetMultiJob(ctx, mj2.JID)
	require.NoError(t, err)
	assert.Nil(t, mj)
	mj, err = p.GetMultiJob(ctx, mj3.JID)
	require.NoError(t, err)
	assert.NotNil(t, mj)

	// deleted all except j5, j7 and j8
	j, err := p.GetByJID(ctx, j1.ClientID, j1.JID)
	require.NoError(t, err)
	assert.Nil(t, j)
	j, err = p.GetByJID(ctx, j2.ClientID, j2.JID)
	require.NoError(t, err)
	assert.Nil(t, j)
	j, err = p.GetByJID(ctx, j3.ClientID, j3.JID)
	require.NoError(t, err)
	assert.Nil(t, j)
	j, err = p.GetByJID(ctx, j4.ClientID, j4.JID)
	require.NoError(t, err)
	assert.Nil(t, j)
	j, err = p.GetByJID(ctx, j5.ClientID, j5.JID)
	require.NoError(t, err)
	assert.NotNil(t, j)
	j, err = p.GetByJID(ctx, j6.ClientID, j6.JID)
	require.NoError(t, err)
	assert.Nil(t, j)
	j, err = p.GetByJID(ctx, j7.ClientID, j7.JID)
	require.NoError(t, err)
	assert.NotNil(t, j)
	j, err = p.GetByJID(ctx, j8.ClientID, j8.JID)
	require.NoError(t, err)
	assert.NotNil(t, j)
}
