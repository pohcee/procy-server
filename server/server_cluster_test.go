package chserver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pohcee/procy-server/server/cluster"
)

func TestOrderedChangesDedupedSessionsFirst(t *testing.T) {
	in := []cluster.Change{
		{Tenant: "a", Entity: cluster.EntityClients, Key: "c1", Op: cluster.OpUpdate},
		{Tenant: "a", Entity: cluster.EntityClients, Key: "c2", Op: cluster.OpUpdate},
		{Tenant: "a", Entity: cluster.EntityAPISessions, Key: "7", Op: cluster.OpInsert},
		{Tenant: "a", Entity: cluster.EntityClients, Key: "c1", Op: cluster.OpUpdate},
		{Tenant: "b", Entity: cluster.EntityClients, Key: "c1", Op: cluster.OpDelete},
		{Tenant: "a", Entity: cluster.EntityAPISessions, Key: "7", Op: cluster.OpDelete},
	}
	assert.Equal(t, []cluster.Change{
		{Tenant: "a", Entity: cluster.EntityAPISessions, Key: "7", Op: cluster.OpDelete},
		{Tenant: "a", Entity: cluster.EntityClients, Key: "c2", Op: cluster.OpUpdate},
		{Tenant: "a", Entity: cluster.EntityClients, Key: "c1", Op: cluster.OpUpdate},
		{Tenant: "b", Entity: cluster.EntityClients, Key: "c1", Op: cluster.OpDelete},
	}, ordered(in))
}
