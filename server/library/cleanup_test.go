package library

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPruneTaskKeepsActiveAndFirst(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	e, err := m.Create(ctx, linuxOnly("Disk", "df 1"), "", "alice")
	require.NoError(t, err)
	for _, code := range []string{"df 2", "df 3", "df 4", "df 5"} {
		e, err = m.Save(ctx, e.ID, SaveInput{Content: linuxOnly("Disk", code), BaseVersion: e.ActiveVersion}, "alice")
		require.NoError(t, err)
	}
	require.Equal(t, 5, e.ActiveVersion)

	require.NoError(t, NewPruneTask(m, 1, 0, nil).Run(ctx))

	vs, err := m.Versions(ctx, e.ID)
	require.NoError(t, err)
	var nums []int
	for _, v := range vs {
		nums = append(nums, v.Version)
	}
	assert.ElementsMatch(t, []int{5, 1}, nums)
}
