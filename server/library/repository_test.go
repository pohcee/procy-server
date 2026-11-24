package library

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRepository serves one entry from memory; every other method is unused.
type fakeRepository struct {
	Repository
	entry Entry
}

func (f *fakeRepository) Get(_ context.Context, id string) (*Entry, error) {
	if id != f.entry.ID {
		return nil, nil
	}
	e := f.entry
	return &e, nil
}

func TestManagerAcceptsRepository(t *testing.T) {
	var r Repository = &fakeRepository{entry: Entry{ID: "e1", Name: "Disk", ActiveVersion: 1}}
	m := NewManager(r, func() time.Time { return t0 })

	e, err := m.Get(testCtx, "e1", false)
	require.NoError(t, err)
	assert.Equal(t, "Disk", e.Name)
}
