package vault

// Exported forms of the value encryption, for tools that copy a vault between stores (the importer re-encrypts the
// values of a passphrase vault for a managed key provider).

// EncryptValue encrypts plain with key (KeySize bytes) the way the vault stores values: AES-256-GCM with the nonce
// prefixed, base64 encoded.
func EncryptValue(plain, key []byte) (string, error) { return encryptValue(plain, key) }

// DecryptValue reverses EncryptValue.
func DecryptValue(encoded string, key []byte) ([]byte, error) { return decryptValue(encoded, key) }

// PassphraseKey is the key a passphrase vault derives from its passphrase (SHA-256 of it).
func PassphraseKey(pass string) []byte { return passphraseKey(pass) }
