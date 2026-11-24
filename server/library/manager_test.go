package library

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "github.com/openrport/openrport/server/api/errors"
)

func newTestManager(t *testing.T) (*Manager, *SQLiteStore) {
	t.Helper()
	s := newTestStore(t)
	now := t0
	return NewManager(s, func() time.Time { now = now.Add(time.Minute); return now }), s
}

// requireStatus fails the test unless err is an APIError with the given HTTP status.
func requireStatus(t *testing.T, err error, status int) {
	t.Helper()
	_ = requireStatusErr(t, err, status)
}

// requireStatusErr is requireStatus that also returns the APIError.
func requireStatusErr(t *testing.T, err error, status int) apierrors.APIError {
	t.Helper()
	require.Error(t, err)
	apiErr, ok := err.(apierrors.APIError)
	require.True(t, ok, "expected APIError, got %T: %v", err, err)
	assert.Equal(t, status, apiErr.HTTPStatus)
	return apiErr
}

func linuxOnly(name, code string) Content {
	return Content{Name: name, Implementations: map[string]Implementation{OSLinux: {Kind: KindCommand, Code: code}}}
}

func TestCreateStartsAtVersion1(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly(" Disk ", "df"), "initial", "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, e.ID)
	assert.Equal(t, "Disk", e.Name)
	assert.Equal(t, 1, e.ActiveVersion)
	assert.Equal(t, []string{OSLinux}, e.OS)
	assert.Equal(t, "alice", e.CreatedBy)
	assert.Equal(t, 60, e.Implementations[OSLinux].TimeoutSec)

	vs, err := m.Versions(ctx, e.ID)
	require.NoError(t, err)
	require.Len(t, vs, 1)
	assert.Equal(t, "initial", vs[0].Message)

	_, err = m.Create(ctx, linuxOnly("Disk", "df -h"), "", "bob")
	apiErr := requireStatusErr(t, err, http.StatusConflict)
	assert.Equal(t, `an entry named "Disk" already exists`, apiErr.Message)

	_, err = m.Create(ctx, Content{Name: "x"}, "", "bob")
	requireStatus(t, err, http.StatusBadRequest)
}

func TestSaveAddsVersionAndChanged(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)

	c := linuxOnly("Disk", "df")
	c.Implementations[OSWindows] = Implementation{Kind: KindCommand, Code: "dir"}
	got, err := m.Save(ctx, e.ID, SaveInput{Content: c, BaseVersion: 1, Message: "add windows"}, "bob")
	require.NoError(t, err)
	assert.Equal(t, 2, got.ActiveVersion)
	assert.Equal(t, []string{OSLinux, OSWindows}, got.OS)
	assert.Equal(t, "bob", got.UpdatedBy)

	v, err := m.Version(ctx, e.ID, 2)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Equal(t, []string{"windows"}, v.Changed)
	assert.Equal(t, "add windows", v.Message)

	_, err = m.Save(ctx, "nope", SaveInput{Content: c, BaseVersion: 1}, "bob")
	requireStatus(t, err, http.StatusNotFound)
	_, err = m.Save(ctx, e.ID, SaveInput{Content: Content{Name: "Disk"}, BaseVersion: 2}, "bob")
	requireStatus(t, err, http.StatusBadRequest)
}

func TestSaveRejectsStaleBaseVersion(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)

	_, err = m.Save(ctx, e.ID, SaveInput{Content: linuxOnly("Disk", "first"), BaseVersion: 1}, "bob")
	require.NoError(t, err)
	_, err = m.Save(ctx, e.ID, SaveInput{Content: linuxOnly("Disk", "second"), BaseVersion: 1}, "carol")
	apiErr := requireStatusErr(t, err, http.StatusConflict)
	assert.Contains(t, apiErr.Message, "version 2")
	assert.Contains(t, apiErr.Message, "bob")
	assert.Contains(t, apiErr.Message, "after version 1 you edited")

	got, err := m.Get(ctx, e.ID, false)
	require.NoError(t, err)
	assert.Equal(t, "first", got.Implementations[OSLinux].Code)
}

func TestSaveWithoutChangeKeepsVersion(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)
	got, err := m.Save(ctx, e.ID, SaveInput{Content: linuxOnly(" Disk", "df"), BaseVersion: 1}, "bob")
	require.NoError(t, err)
	assert.Equal(t, 1, got.ActiveVersion)
	vs, err := m.Versions(ctx, e.ID)
	require.NoError(t, err)
	assert.Len(t, vs, 1)
}

func TestRestoreCreatesNewVersion(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)
	_, err = m.Save(ctx, e.ID, SaveInput{Content: linuxOnly("Disk", "df -h"), BaseVersion: 1}, "bob")
	require.NoError(t, err)

	got, err := m.Restore(ctx, e.ID, 1, 2, "carol")
	require.NoError(t, err)
	assert.Equal(t, 3, got.ActiveVersion)
	assert.Equal(t, "df", got.Implementations[OSLinux].Code)
	v, err := m.Version(ctx, e.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, "Restored version 1", v.Message)
	v1, err := m.Version(ctx, e.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, *v1.Content, *v.Content)

	_, err = m.Restore(ctx, e.ID, 99, 3, "carol")
	requireStatus(t, err, http.StatusNotFound)
	_, err = m.Restore(ctx, e.ID, 2, 2, "carol")
	requireStatus(t, err, http.StatusConflict)
}

func TestDeleteHidesAndUndeleteRestores(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)
	require.NoError(t, m.Delete(ctx, e.ID, "bob"))

	_, err = m.Get(ctx, e.ID, false)
	requireStatus(t, err, http.StatusNotFound)
	got, err := m.Get(ctx, e.ID, true)
	require.NoError(t, err)
	require.NotNil(t, got.DeletedAt)
	assert.Equal(t, "bob", got.DeletedBy)

	list, total, err := m.List(ctx, ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list)
	assert.Equal(t, 0, total)
	yes := true
	list, _, err = m.List(ctx, ListOptions{Deleted: &yes})
	require.NoError(t, err)
	assert.Len(t, list, 1)

	_, err = m.Save(ctx, e.ID, SaveInput{Content: linuxOnly("Disk", "x"), BaseVersion: 1}, "bob")
	requireStatus(t, err, http.StatusNotFound)
	requireStatus(t, m.Delete(ctx, e.ID, "bob"), http.StatusNotFound)
	requireStatus(t, m.Delete(ctx, "nope", "bob"), http.StatusNotFound)

	back, err := m.Undelete(ctx, e.ID, "bob")
	require.NoError(t, err)
	assert.Nil(t, back.DeletedAt)
	_, err = m.Get(ctx, e.ID, false)
	require.NoError(t, err)
}

func TestUndeleteNameTaken(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)
	require.NoError(t, m.Delete(ctx, e.ID, "bob"))
	_, err = m.Create(ctx, linuxOnly("Disk", "other"), "", "alice")
	require.NoError(t, err)

	_, err = m.Undelete(ctx, e.ID, "bob")
	requireStatus(t, err, http.StatusConflict)
	_, err = m.Undelete(ctx, "nope", "bob")
	requireStatus(t, err, http.StatusNotFound)
}

func TestBuiltinIsReadOnly(t *testing.T) {
	m, s := newTestManager(t)
	ctx := testCtx
	e, v := testEntry("b1", "Builtin")
	key := "builtin.one"
	e.BuiltinKey = &key
	require.NoError(t, s.Insert(ctx, e, v))

	_, err := m.Save(ctx, "b1", SaveInput{Content: linuxOnly("Builtin", "x"), BaseVersion: 1}, "bob")
	requireStatus(t, err, http.StatusForbidden)
	_, err = m.Restore(ctx, "b1", 1, 1, "bob")
	requireStatus(t, err, http.StatusForbidden)
	requireStatus(t, m.Delete(ctx, "b1", "bob"), http.StatusForbidden)
}

func TestListFiltersByOS(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	_, err := m.Create(ctx, linuxOnly("Alpha", "a"), "", "u")
	require.NoError(t, err)
	both := linuxOnly("Beta", "b")
	both.Tags = []string{"net"}
	both.Implementations[OSWindows] = Implementation{Code: "b"}
	_, err = m.Create(ctx, both, "", "u")
	require.NoError(t, err)

	names := func(o ListOptions) []string {
		list, _, err := m.List(ctx, o)
		require.NoError(t, err)
		var n []string
		for _, e := range list {
			n = append(n, e.Name)
		}
		return n
	}
	assert.Equal(t, []string{"Alpha", "Beta"}, names(ListOptions{}))
	assert.Equal(t, []string{"Beta"}, names(ListOptions{OS: OSWindows}))
	assert.Equal(t, []string{"Beta"}, names(ListOptions{Tag: "net"}))
	assert.Equal(t, []string{"Alpha"}, names(ListOptions{Name: "alp"}))
	assert.Equal(t, []string{"Beta", "Alpha"}, names(ListOptions{Sort: "-updated_at"}))

	list, total, err := m.List(ctx, ListOptions{Limit: 1, Offset: 1})
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, list, 1)
	assert.Equal(t, "Beta", list[0].Name)
}

type fakeRefs struct{ refs []Reference }

func (f fakeRefs) SchedulesUsing(_ context.Context, _ string) ([]Reference, error) {
	return f.refs, nil
}

func TestDeleteRefusedWhenScheduled(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)
	m.SetReferenceChecker(fakeRefs{[]Reference{{ID: "s1", Name: "nightly"}, {ID: "s2", Name: "weekly"}}})

	err = m.Delete(ctx, e.ID, "bob")
	apiErr := requireStatusErr(t, err, http.StatusConflict)
	assert.Equal(t, `entry "Disk" is used by schedules: nightly, weekly`, apiErr.Message)
	got, err := m.Get(ctx, e.ID, false)
	require.NoError(t, err)
	assert.Nil(t, got.DeletedAt)
}

func TestUsedByListsSchedules(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)
	refs, err := m.UsedBy(ctx, e.ID)
	require.NoError(t, err)
	assert.Empty(t, refs)

	m.SetReferenceChecker(fakeRefs{[]Reference{{ID: "s1", Name: "nightly"}}})
	refs, err = m.UsedBy(ctx, e.ID)
	require.NoError(t, err)
	assert.Equal(t, []Reference{{ID: "s1", Name: "nightly"}}, refs)

	_, err = m.UsedBy(ctx, "nope")
	requireStatus(t, err, http.StatusNotFound)
}

func TestDeleteAllowedWithoutReferences(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)
	m.SetReferenceChecker(fakeRefs{})
	require.NoError(t, m.Delete(ctx, e.ID, "bob"))
}
