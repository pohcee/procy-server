package chconfig

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// minJWTSecretLength is the shortest jwt_secret accepted at startup.
const minJWTSecretLength = 16

// exampleValues are placeholders and sample values shipped in rportd.example.conf or the docs.
// Running with any of them means the install was never configured properly.
var exampleValues = map[string]string{
	"<YOUR_SECRET>":      "a placeholder from rportd.example.conf",
	"<YOUR_SEED>":        "a placeholder from rportd.example.conf",
	"quei1too2Jae3xootu": "an example value from the documentation",
	"admin:foobaz":       "the example credentials from rportd.example.conf",
	"clientAuth1:1234":   "the example credentials from rportd.example.conf",
}

// checkNotExampleValue returns an error when value is a known placeholder or example value.
func checkNotExampleValue(setting, value string) error {
	if where, ok := exampleValues[value]; ok {
		return fmt.Errorf("%s is %s; set your own value", setting, where)
	}
	return nil
}

// validateJWTSecret rejects example and too-short API JWT secrets.
func validateJWTSecret(secret string) error {
	if err := checkNotExampleValue("jwt_secret", secret); err != nil {
		return err
	}
	if len(secret) < minJWTSecretLength {
		return fmt.Errorf("jwt_secret is too short (%d characters); use at least %d, ideally 32 or more (e.g. openssl rand -hex 32)",
			len(secret), minJWTSecretLength)
	}
	return nil
}

// loadOrCreateJWTSecret returns the JWT secret saved in dataDir, generating and saving one first if
// there is none. Without this, every restart generated a new secret and logged everyone out.
func loadOrCreateJWTSecret(dataDir string) (string, error) {
	path := filepath.Join(dataDir, jwtSecretFileName)

	data, err := os.ReadFile(path)
	if err == nil {
		secret := strings.TrimSpace(string(data))
		if err := validateJWTSecret(secret); err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		return secret, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("can't read API JWT secret: %w", err)
	}

	secret, err := generateJWTSecret()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
		return "", fmt.Errorf("can't save API JWT secret: %w", err)
	}
	return secret, nil
}

// generateJWTSecret returns 32 random bytes, hex encoded.
func generateJWTSecret() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("can't generate API JWT secret: %s", err)
	}
	return hex.EncodeToString(data), nil
}
