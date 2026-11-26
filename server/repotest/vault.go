package repotest

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/vault"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

// vaultT0 has a fraction of a second: values are stored with second precision.
var vaultT0 = time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)

func vaultInput(clientID, key, value string) *vault.InputValue {
	return &vault.InputValue{ClientID: clientID, RequiredGroup: "g1", Key: key, Value: value, Type: vault.SecretType}
}

func vaultSave(ctx context.Context, t *testing.T, r vault.DbProvider, user string, val *vault.InputValue, at time.Time) int {
	t.Helper()
	id, err := r.Save(ctx, user, 0, val, at)
	require.NoError(t, err)
	require.NotZero(t, id)
	return int(id)
}

func vaultKeys(vals []vault.ValueKey) []string {
	keys := []string{}
	for _, v := range vals {
		keys = append(keys, v.ClientID+"/"+v.Key)
	}
	return keys
}

func vaultList(ctx context.Context, t *testing.T, r vault.DbProvider, lo *query.ListOptions) []string {
	t.Helper()
	vals, err := r.List(ctx, lo)
	require.NoError(t, err)
	return vaultKeys(vals)
}

// Vault runs the contract tests of vault.DbProvider: the status and values of each tenant's vault.
func Vault(t *testing.T, newRepo Factory[vault.DbProvider]) {
	a, b := Ctx(TenantA), Ctx(TenantB)

	t.Run("RequiresTenant", func(t *testing.T) {
		r := newRepo(t)
		ctx := context.Background()

		_, err := r.GetStatus(ctx)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		assert.ErrorIs(t, r.SetStatus(ctx, vault.DbStatus{StatusName: vault.DbStatusInit}), tenant.ErrMissing)
		_, _, err = r.GetByID(ctx, 1)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = r.List(ctx, &query.ListOptions{})
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, _, err = r.FindByKeyAndClientID(ctx, "k", "c")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = r.Save(ctx, "u", 0, vaultInput("c", "k", "v"), vaultT0)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		assert.ErrorIs(t, r.Delete(ctx, 1), tenant.ErrMissing)

		// nothing was written for any tenant
		vals, err := r.List(a, &query.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, vals)
		st, err := r.GetStatus(a)
		require.NoError(t, err)
		assert.Equal(t, "", st.StatusName)
	})

	t.Run("StatusEmptyUntilSet", func(t *testing.T) {
		r := newRepo(t)
		st, err := r.GetStatus(a)
		require.NoError(t, err)
		assert.Equal(t, "", st.StatusName)
		assert.Equal(t, "", st.EncCheckValue)
		assert.Equal(t, "", st.DecCheckValue)
	})

	t.Run("SetStatusInsertsThenUpdates", func(t *testing.T) {
		r := newRepo(t)
		require.NoError(t, r.SetStatus(a, vault.DbStatus{StatusName: vault.DbStatusInit, EncCheckValue: "123", DecCheckValue: "345"}))
		st, err := r.GetStatus(a)
		require.NoError(t, err)
		assert.Equal(t, vault.DbStatusInit, st.StatusName)
		assert.Equal(t, "123", st.EncCheckValue)
		assert.Equal(t, "345", st.DecCheckValue)

		require.NoError(t, r.SetStatus(a, vault.DbStatus{StatusName: vault.DbStatusNotInit, EncCheckValue: "678", DecCheckValue: "91011"}))
		st, err = r.GetStatus(a)
		require.NoError(t, err)
		assert.Equal(t, vault.DbStatusNotInit, st.StatusName)
		assert.Equal(t, "678", st.EncCheckValue)
		assert.Equal(t, "91011", st.DecCheckValue)
	})

	t.Run("StatusKeepsRawCheckBytes", func(t *testing.T) {
		r := newRepo(t)
		// the passphrase manager's dec_check is a timestamp followed by raw SHA-256 bytes, not valid UTF-8
		raw := "1767323045000000000\xe3\xb0\xc4\x42\x98\xfc\x1c\x14\x9a\xfb\xf4\xc8\x99\x6f\xb9\x24"
		require.NoError(t, r.SetStatus(a, vault.DbStatus{StatusName: vault.DbStatusInit, EncCheckValue: "enc", DecCheckValue: raw}))
		st, err := r.GetStatus(a)
		require.NoError(t, err)
		assert.Equal(t, raw, st.DecCheckValue)
	})

	t.Run("StatusPerTenant", func(t *testing.T) {
		r := newRepo(t)
		require.NoError(t, r.SetStatus(a, vault.DbStatus{StatusName: vault.DbStatusInit, EncCheckValue: "encA", DecCheckValue: "decA"}))

		st, err := r.GetStatus(b)
		require.NoError(t, err)
		assert.Equal(t, "", st.StatusName, "B's vault is not initialized by A's")

		require.NoError(t, r.SetStatus(b, vault.DbStatus{StatusName: vault.DbStatusInit, EncCheckValue: "encB", DecCheckValue: "decB"}))
		require.NoError(t, r.SetStatus(b, vault.DbStatus{StatusName: vault.DbStatusNotInit, EncCheckValue: "encB2", DecCheckValue: "decB2"}))

		st, err = r.GetStatus(a)
		require.NoError(t, err)
		assert.Equal(t, vault.DbStatus{ID: st.ID, StatusName: vault.DbStatusInit, EncCheckValue: "encA", DecCheckValue: "decA"}, st)
		st, err = r.GetStatus(b)
		require.NoError(t, err)
		assert.Equal(t, vault.DbStatus{ID: st.ID, StatusName: vault.DbStatusNotInit, EncCheckValue: "encB2", DecCheckValue: "decB2"}, st)
	})

	t.Run("SaveInsertsAndGetByID", func(t *testing.T) {
		r := newRepo(t)
		id1 := vaultSave(a, t, r, "u1", vaultInput("c1", "k1", "v1"), vaultT0)
		id2 := vaultSave(a, t, r, "u1", vaultInput("c2", "k2", "v2"), vaultT0)
		assert.NotEqual(t, id1, id2)

		got, found, err := r.GetByID(a, id1)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, id1, got.ID)
		assert.Equal(t, vault.InputValue{ClientID: "c1", RequiredGroup: "g1", Key: "k1", Value: "v1", Type: vault.SecretType}, got.InputValue)
		assert.Equal(t, "u1", got.CreatedBy)
		require.NotNil(t, got.UpdatedBy)
		assert.Equal(t, "u1", *got.UpdatedBy)
		want := vaultT0.Truncate(time.Second)
		assert.True(t, want.Equal(got.CreatedAt), "created_at %v, want %v", got.CreatedAt, want)
		assert.True(t, want.Equal(got.UpdatedAt), "updated_at %v, want %v", got.UpdatedAt, want)

		_, found, err = r.GetByID(a, -2)
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("SaveRoundTripsEmptyFields", func(t *testing.T) {
		r := newRepo(t)
		in := &vault.InputValue{ClientID: "", RequiredGroup: "", Key: "k", Value: "", Type: vault.TextType}
		id := vaultSave(a, t, r, "u1", in, vaultT0)
		got, found, err := r.GetByID(a, id)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, *in, got.InputValue)

		got, found, err = r.FindByKeyAndClientID(a, "k", "")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, id, got.ID)
	})

	t.Run("SaveUpdates", func(t *testing.T) {
		r := newRepo(t)
		id := vaultSave(a, t, r, "u1", vaultInput("c1", "k1", "v1"), vaultT0)
		later := vaultT0.Add(time.Hour)
		upd := &vault.InputValue{ClientID: "c9", RequiredGroup: "g9", Key: "k9", Value: "v9", Type: vault.MarkdownType}
		gotID, err := r.Save(a, "u2", int64(id), upd, later)
		require.NoError(t, err)
		assert.Equal(t, int64(id), gotID)

		got, found, err := r.GetByID(a, id)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, *upd, got.InputValue)
		assert.Equal(t, "u1", got.CreatedBy)
		require.NotNil(t, got.UpdatedBy)
		assert.Equal(t, "u2", *got.UpdatedBy)
		assert.True(t, vaultT0.Truncate(time.Second).Equal(got.CreatedAt))
		assert.True(t, later.Truncate(time.Second).Equal(got.UpdatedAt))

		_, found, err = r.FindByKeyAndClientID(a, "k1", "c1")
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("KeyUniquePerClient", func(t *testing.T) {
		r := newRepo(t)
		vaultSave(a, t, r, "u1", vaultInput("c1", "k1", "v1"), vaultT0)
		_, err := r.Save(a, "u1", 0, vaultInput("c1", "k1", "other"), vaultT0)
		assert.Error(t, err, "a key exists once per client")

		vaultSave(a, t, r, "u1", vaultInput("c2", "k1", "v2"), vaultT0)
		vaultSave(a, t, r, "u1", vaultInput("", "k1", "v3"), vaultT0)

		got, found, err := r.FindByKeyAndClientID(a, "k1", "c1")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "v1", got.Value)
	})

	t.Run("FindByKeyAndClientID", func(t *testing.T) {
		r := newRepo(t)
		id := vaultSave(a, t, r, "u1", vaultInput("c1", "k1", "v1"), vaultT0)
		vaultSave(a, t, r, "u1", vaultInput("c2", "k2", "v2"), vaultT0)

		_, found, err := r.FindByKeyAndClientID(a, "k1", "unknown")
		require.NoError(t, err)
		assert.False(t, found)
		_, found, err = r.FindByKeyAndClientID(a, "unknown", "c1")
		require.NoError(t, err)
		assert.False(t, found)
		_, found, err = r.FindByKeyAndClientID(a, "k1", "c2")
		require.NoError(t, err)
		assert.False(t, found)

		got, found, err := r.FindByKeyAndClientID(a, "k1", "c1")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, id, got.ID)
		assert.Equal(t, vault.InputValue{ClientID: "c1", RequiredGroup: "g1", Key: "k1", Value: "v1", Type: vault.SecretType}, got.InputValue)
		assert.Equal(t, "u1", got.CreatedBy)
		assert.True(t, vaultT0.Truncate(time.Second).Equal(got.CreatedAt))
	})

	t.Run("List", func(t *testing.T) {
		r := newRepo(t)
		id1 := vaultSave(a, t, r, "u1", vaultInput("c2", "kb", "v1"), vaultT0)
		vaultSave(a, t, r, "u2", vaultInput("c1", "ka", "v2"), vaultT0.Add(time.Hour))
		vaultSave(a, t, r, "u1", vaultInput("c3", "kc", "v3"), vaultT0.Add(2*time.Hour))

		vals, err := r.List(a, &query.ListOptions{})
		require.NoError(t, err)
		require.Len(t, vals, 3)
		assert.ElementsMatch(t, []string{"c2/kb", "c1/ka", "c3/kc"}, vaultKeys(vals), "without sorts, in no particular order")
		for _, v := range vals {
			if v.ID == id1 {
				assert.Equal(t, vault.ValueKey{ID: id1, ClientID: "c2", CreatedBy: "u1", CreatedAt: v.CreatedAt, Key: "kb"}, v)
				assert.True(t, vaultT0.Truncate(time.Second).Equal(v.CreatedAt))
			}
		}

		assert.Equal(t, []string{"c3/kc", "c2/kb", "c1/ka"}, vaultList(a, t, r, &query.ListOptions{
			Sorts: []query.SortOption{{Column: "client_id", IsASC: false}},
		}))
		assert.Equal(t, []string{"c3/kc", "c1/ka", "c2/kb"}, vaultList(a, t, r, &query.ListOptions{
			Sorts: []query.SortOption{{Column: "created_at", IsASC: false}},
		}))
		assert.Equal(t, []string{"c3/kc", "c1/ka", "c2/kb"}, vaultList(a, t, r, &query.ListOptions{
			Sorts: []query.SortOption{{Column: "id", IsASC: false}},
		}))
		assert.Equal(t, []string{"c1/ka", "c2/kb", "c3/kc"}, vaultList(a, t, r, &query.ListOptions{
			Sorts: []query.SortOption{{Column: "created_by", IsASC: false}, {Column: "client_id", IsASC: true}},
		}))

		assert.Equal(t, []string{"c3/kc", "c2/kb"}, vaultList(a, t, r, &query.ListOptions{
			Sorts:   []query.SortOption{{Column: "key", IsASC: false}},
			Filters: []query.FilterOption{{Column: []string{"created_by"}, Values: []string{"u1"}}},
		}))
		assert.Equal(t, []string{}, vaultList(a, t, r, &query.ListOptions{
			Filters: []query.FilterOption{{Column: []string{"client_id"}, Values: []string{"unknown"}}},
		}))
		assert.Equal(t, []string{"c1/ka"}, vaultList(a, t, r, &query.ListOptions{
			Filters: []query.FilterOption{{Column: []string{"key"}, Values: []string{"ka"}}},
		}))
		assert.Equal(t, []string{"c2/kb"}, vaultList(a, t, r, &query.ListOptions{
			Filters: []query.FilterOption{
				{Column: []string{"key"}, Values: []string{"ka", "kb"}},
				{Column: []string{"created_by"}, Values: []string{"u1"}},
			},
		}))
		assert.Equal(t, []string{"c2/kb"}, vaultList(a, t, r, &query.ListOptions{
			Filters: []query.FilterOption{{Column: []string{"client_id", "key"}, Values: []string{"c2"}}},
		}))
		// a wildcard pattern matches case-insensitively
		assert.Equal(t, []string{"c2/kb", "c3/kc"}, vaultList(a, t, r, &query.ListOptions{
			Sorts:   []query.SortOption{{Column: "key", IsASC: true}},
			Filters: []query.FilterOption{{Column: []string{"key"}, Values: []string{"K*"}}, {Column: []string{"created_by"}, Values: []string{"u1"}}},
		}))
		assert.Equal(t, []string{"c2/kb"}, vaultList(a, t, r, &query.ListOptions{
			Filters: []query.FilterOption{{Column: []string{"id"}, Values: []string{strconv.Itoa(id1)}}},
		}))
	})

	t.Run("ListEmptyIsNotNil", func(t *testing.T) {
		r := newRepo(t)
		vals, err := r.List(a, &query.ListOptions{})
		require.NoError(t, err)
		assert.NotNil(t, vals)
		assert.Empty(t, vals)
	})

	t.Run("Delete", func(t *testing.T) {
		r := newRepo(t)
		id1 := vaultSave(a, t, r, "u1", vaultInput("c1", "k1", "v1"), vaultT0)
		id2 := vaultSave(a, t, r, "u1", vaultInput("c2", "k2", "v2"), vaultT0)

		assert.Error(t, r.Delete(a, -2))
		require.NoError(t, r.Delete(a, id1))
		assert.Error(t, r.Delete(a, id1), "deleted twice")

		_, found, err := r.GetByID(a, id1)
		require.NoError(t, err)
		assert.False(t, found)
		_, found, err = r.GetByID(a, id2)
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, []string{"c2/k2"}, vaultList(a, t, r, &query.ListOptions{}))

		// the key can be stored again
		vaultSave(a, t, r, "u1", vaultInput("c1", "k1", "v1"), vaultT0)
	})

	t.Run("TenantIsolation", func(t *testing.T) {
		r := newRepo(t)
		idA := vaultSave(a, t, r, "ua", vaultInput("c1", "k1", "va"), vaultT0)
		idB := vaultSave(b, t, r, "ub", vaultInput("c1", "k1", "vb"), vaultT0)
		assert.NotEqual(t, idA, idB)

		got, found, err := r.FindByKeyAndClientID(a, "k1", "c1")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "va", got.Value)
		got, found, err = r.FindByKeyAndClientID(b, "k1", "c1")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "vb", got.Value)

		// B can't read, change or delete A's value by its id
		_, found, err = r.GetByID(b, idA)
		require.NoError(t, err)
		assert.False(t, found)
		_, _ = r.Save(b, "ub", int64(idA), vaultInput("c1", "k1", "changed by B"), vaultT0.Add(time.Hour))
		assert.Error(t, r.Delete(b, idA))

		got, found, err = r.GetByID(a, idA)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "va", got.Value)
		assert.Equal(t, "ua", *got.UpdatedBy)
		assert.True(t, vaultT0.Truncate(time.Second).Equal(got.UpdatedAt))

		vals, err := r.List(b, &query.ListOptions{})
		require.NoError(t, err)
		require.Len(t, vals, 1)
		assert.Equal(t, idB, vals[0].ID)
		assert.Equal(t, "ub", vals[0].CreatedBy)

		require.NoError(t, r.Delete(a, idA))
		_, found, err = r.GetByID(b, idB)
		require.NoError(t, err)
		assert.True(t, found, "deleting A's value leaves B's")
	})
}
