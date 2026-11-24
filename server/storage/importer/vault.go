package importer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"

	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/tenant"
)

// vaultSource is the source vault, read before anything is written.
type vaultSource struct {
	status *vaultStatus
	values [][]any // per value: the columns of vaultValueCols, value encrypted as it will be written
}

type vaultStatus struct {
	ID       int64
	DBStatus string
	EncCheck sql.NullString
	DecCheck []byte // raw bytes (a timestamp and a SHA-256 sum), bytea in Postgres
}

var vaultValueCols = []col{
	c("id", kInt), c("client_id", kText), c("required_group", kNText), c("created_at", kTime), c("created_by", kText),
	c("updated_at", kTime), c("updated_by", kNText), c("key", kText), c("value", kText), c("type", kText),
}

const vaultValueIndex = 8 // of "value" in vaultValueCols

// prepareVault reads the source vault. For a managed target it checks the passphrase against the vault's check
// value (as the server's unlock does) and re-encrypts every value with the target key, so a wrong or missing
// passphrase fails before anything is written.
func (im *importer) prepareVault(ctx context.Context) error {
	d := im.dbs[fileVault]
	if d == nil {
		return nil
	}
	v := &vaultSource{}
	im.vault = v
	if ok, err := hasTable(d.db, "status"); err != nil {
		return err
	} else if ok {
		st := &vaultStatus{}
		err := d.db.QueryRow("SELECT id, db_status, enc_check, CAST(dec_check AS BLOB) FROM status ORDER BY id LIMIT 1").
			Scan(&st.ID, &st.DBStatus, &st.EncCheck, &st.DecCheck)
		if err == nil {
			v.status = st
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s status: %w", d.name, err)
		}
	}
	if ok, err := hasTable(d.db, "values"); err != nil || !ok {
		return err
	}
	if err := readRows(d, "values", vaultValueCols, "id", func(vals []any) error {
		v.values = append(v.values, vals)
		return nil
	}); err != nil {
		return err
	}
	if im.o.VaultKeys == nil || len(v.values) == 0 {
		return nil
	}

	// managed target: decrypt with the source passphrase, encrypt with the target key
	if v.status == nil || v.status.DBStatus != vault.DbStatusInit || !v.status.EncCheck.Valid || v.status.EncCheck.String == "" {
		return fmt.Errorf("the source vault has %d values but is not set up with a passphrase: its values can't be decrypted", len(v.values))
	}
	if im.o.VaultPassphrase == "" {
		return fmt.Errorf("the source vault has %d values: the source vault passphrase is needed to re-encrypt them for the target's vault key", len(v.values))
	}
	pass := vault.PassphraseKey(im.o.VaultPassphrase)
	if _, err := vault.DecryptValue(v.status.EncCheck.String, pass); err != nil {
		return errors.New("the vault passphrase does not match the source vault")
	}
	key, err := im.targetVaultKey(ctx)
	if err != nil {
		return err
	}
	for _, vals := range v.values {
		plain, err := vault.DecryptValue(vals[vaultValueIndex].(string), pass)
		if err != nil {
			return fmt.Errorf("vault value %d can't be decrypted with the passphrase: %w", vals[0], err)
		}
		enc, err := vault.EncryptValue(plain, key)
		if err != nil {
			return fmt.Errorf("vault value %d: %w", vals[0], err)
		}
		vals[vaultValueIndex] = enc
	}
	return nil
}

// targetVaultKey returns the tenant's managed vault key. A dry run re-encrypts with a throwaway key instead: asking
// the provider would create and store the tenant's key, and a dry run writes nothing.
func (im *importer) targetVaultKey(ctx context.Context) ([]byte, error) {
	if im.o.DryRun {
		key := make([]byte, vault.KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		return key, nil
	}
	key, err := im.o.VaultKeys.Key(tenant.WithID(ctx, im.tid))
	if err != nil {
		return nil, fmt.Errorf("target vault key: %w", err)
	}
	return key, nil
}

// writeVault writes the vault. Passphrase target: the status and the values as they are, so the source passphrase
// unlocks the target vault; a target vault already set up with another passphrase is refused. Managed target: the
// re-encrypted values and no status (a passphrase status would make the server refuse the tenant's vault); a target
// vault set up with a passphrase is refused.
func (im *importer) writeVault(w *writer) error {
	vc, sc := im.count(entVaultValues), im.count(entVaultStatus)
	v := im.vault
	if v == nil {
		return nil
	}
	var target struct {
		DBStatus string         `db:"db_status"`
		EncCheck sql.NullString `db:"enc_check"`
	}
	err := w.tx.GetContext(w.ctx, &target, "SELECT db_status, enc_check FROM vault_status WHERE tenant_id = $1", string(w.tid))
	hasTarget := err == nil
	if err != nil && !isNoRows(err) {
		return err
	}
	targetPassphrase := hasTarget && (target.DBStatus == vault.DbStatusInit || target.EncCheck.String != "")

	if v.status != nil {
		sc.Read++
	}
	switch {
	case im.o.VaultKeys != nil:
		if targetPassphrase && len(v.values) > 0 {
			return fmt.Errorf("the vault of tenant %s is set up with a passphrase: values for its managed vault key can't be added", w.tid)
		}
		if v.status != nil {
			sc.Skipped++
			im.rep.warnf("vault status: not copied, the target's vault key is managed (no passphrase)")
		}
	case v.status != nil:
		if hasTarget {
			if target.EncCheck.String != v.status.EncCheck.String {
				return fmt.Errorf("the vault of tenant %s is set up with another passphrase: values encrypted with two passphrases can't be mixed", w.tid)
			}
			sc.Present++
			break
		}
		if _, err := w.insertOne("vault_status", []string{"id", "db_status", "enc_check", "dec_check"},
			[]any{v.status.ID, v.status.DBStatus, nullString(v.status.EncCheck), v.status.DecCheck}); err != nil {
			return err
		}
		sc.Added++
	case len(v.values) > 0:
		return fmt.Errorf("the source vault has %d values but no status: it can't be copied", len(v.values))
	}

	taken := 0
	for _, vals := range v.values {
		vc.Read++
		added, err := w.insertOne("vault_values", names(vaultValueCols), vals)
		if err != nil {
			return err
		}
		if added {
			vc.Added++
			continue
		}
		here, err := w.exists("SELECT 1 FROM vault_values WHERE tenant_id = $1 AND id = $2 AND client_id = $3 AND key = $4", vals[0], vals[1], vals[7])
		if err != nil {
			return err
		}
		if here {
			vc.Present++
		} else {
			vc.Skipped++
			taken++
		}
	}
	if taken > 0 {
		im.rep.warnf("vault values: %d values whose id or client and key another value of the target has, not imported", taken)
	}
	return nil
}

func nullString(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}
