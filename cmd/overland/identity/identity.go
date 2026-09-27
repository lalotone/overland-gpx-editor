// Package identity turns a signed-in account into the opaque owner key the
// backend stores private data under.
//
// The key is an HMAC of the account's random WebAuthn handle under a secret
// kept beside the account database. The backend never sees the handle, the
// username or the secret, so a data directory on its own cannot be linked
// back to accounts; that takes both accounts.db and owner.key. Losing
// owner.key orphans every user's data, because the keys cannot be recomputed
// without it: back the two files up together.
package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lalotone/overland-gpx-editor/internal/passkeyauth"
	"github.com/lalotone/overland-gpx-editor/internal/server"
)

// KeyFile is the secret's file name, next to the account database.
const KeyFile = "owner.key"

const (
	secretBytes = 32
	// Versioned so a future derivation change cannot silently collide.
	derivationLabel = "overland-owner-v1"
)

// KeyPath returns the secret's path for an account database path.
func KeyPath(authDB string) string {
	return filepath.Join(filepath.Dir(authDB), KeyFile)
}

// Secret derives owner keys. Zero-valued it is unusable.
type Secret struct {
	key []byte
}

// LoadOrCreate reads the secret at path, creating a random one (mode 0600,
// in a 0700 directory) when there is none. A key that exists but cannot be
// parsed is an error, never replaced: replacing it would orphan every owner.
func LoadOrCreate(path string) (Secret, error) {
	secret, err := Load(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return secret, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Secret{}, err
	}
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Secret{}, err
	}
	encoded := []byte(hex.EncodeToString(raw) + "\n")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		// Another process created it first; use theirs.
		return Load(path)
	}
	if err != nil {
		return Secret{}, err
	}
	if _, err := f.Write(encoded); err != nil {
		f.Close()
		os.Remove(path)
		return Secret{}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return Secret{}, err
	}
	return Secret{key: raw}, nil
}

// Load reads an existing secret. It returns os.ErrNotExist when there is none.
func Load(path string) (Secret, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Secret{}, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != secretBytes {
		return Secret{}, fmt.Errorf("%s is not a valid owner key", path)
	}
	return Secret{key: key}, nil
}

// Owner returns the account's owner key. It depends only on the handle,
// which is random and survives a rename, never on the username or ID.
func (s Secret) Owner(account passkeyauth.Account) (server.Owner, error) {
	if len(s.key) != secretBytes {
		return "", errors.New("owner key is not loaded")
	}
	if len(account.Handle) == 0 {
		return "", errors.New("account has no handle")
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(derivationLabel))
	mac.Write([]byte{0})
	mac.Write(account.Handle)
	owner := server.Owner(strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(mac.Sum(nil))))
	if !owner.Valid() {
		return "", errors.New("derived owner key is malformed")
	}
	return owner, nil
}
