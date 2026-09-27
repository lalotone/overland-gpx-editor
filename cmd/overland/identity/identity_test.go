package identity

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalotone/overland-gpx-editor/internal/passkeyauth"
)

func account(name string) passkeyauth.Account {
	handle := make([]byte, 64)
	rand.Read(handle)
	return passkeyauth.Account{ID: 7, Handle: handle, Username: name}
}

func TestOwnerKeyRevealsNothingAndSurvivesRename(t *testing.T) {
	secret, err := LoadOrCreate(filepath.Join(t.TempDir(), "owner.key"))
	if err != nil {
		t.Fatal(err)
	}
	alice := account("alice")
	owner, err := secret.Owner(alice)
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Valid() || owner == "local" {
		t.Fatalf("owner = %q", owner)
	}
	key := string(owner)
	if strings.Contains(key, "alice") || strings.Contains(key, "7") && strings.Contains(key, "alice") {
		t.Fatalf("owner key names the account: %q", key)
	}
	if bytes.Contains([]byte(key), alice.Handle[:8]) {
		t.Fatalf("owner key contains the handle")
	}
	renamed := alice
	renamed.Username, renamed.ID = "alicia", 99
	if again, _ := secret.Owner(renamed); again != owner {
		t.Fatalf("rename changed the owner: %q != %q", again, owner)
	}
	if other, _ := secret.Owner(account("alice")); other == owner {
		t.Fatal("two accounts share an owner key")
	}
	if _, err := (Secret{}).Owner(alice); err == nil {
		t.Fatal("unloaded secret derived a key")
	}
}

func TestSecretIsCreatedOnceWithPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "owner.key")
	first, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	second, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	acct := account("bob")
	a, _ := first.Owner(acct)
	b, _ := second.Owner(acct)
	if a != b {
		t.Fatal("reloaded secret derives a different owner")
	}
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil {
		t.Fatal("a corrupt key was replaced instead of refused")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing key error = %v", err)
	}
}

func TestKeyPathSitsBesideTheAccountDatabase(t *testing.T) {
	if got := KeyPath("/etc/overland/accounts.db"); got != "/etc/overland/owner.key" {
		t.Fatalf("KeyPath = %q", got)
	}
}
