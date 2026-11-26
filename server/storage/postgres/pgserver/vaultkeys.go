package pgserver

import (
	"context"
	"fmt"
	"io"

	kms "cloud.google.com/go/kms/apiv1"
	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/storage/postgres/vaultkeys"
	"github.com/pohcee/procy-server/server/vault/vaultkms"
	"github.com/pohcee/procy-server/share/tenant"
)

// KMSVaultKeys returns the managed vault key provider that wraps every tenant's vault key with the Cloud KMS
// CryptoKey keyName (projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>), the wrapped keys stored in db. closeFn
// releases the KMS client.
func KMSVaultKeys(ctx context.Context, keyName string, db *sqlx.DB, logw io.Writer) (_ *vaultkms.Provider, closeFn func(), _ error) {
	client, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("vault: Cloud KMS client: %w", err)
	}
	p, err := vaultKeys(client, keyName, db, logw)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return p, func() { _ = client.Close() }, nil
}

func vaultKeys(k vaultkms.KMS, keyName string, db *sqlx.DB, logw io.Writer) (*vaultkms.Provider, error) {
	return vaultkms.New(vaultkms.Config{
		KMS:     k,
		Store:   vaultkeys.New(db),
		KeyName: func(tenant.ID) (string, error) { return keyName, nil },
		OnError: func(tid tenant.ID, err error) { fmt.Fprintf(logw, "vault key of tenant %s: %v\n", tid, err) },
	})
}
