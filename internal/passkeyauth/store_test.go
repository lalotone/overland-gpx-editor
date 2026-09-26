package passkeyauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "sub", "accounts.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestOpenAccountsPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	store, err := OpenStore(filepath.Join(dir, "accounts.db"))
	require.NoError(t, err)
	require.NoError(t, store.Close())
	info, err := os.Stat(filepath.Join(dir, "accounts.db"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	info, err = os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	_, err = OpenStore(filepath.Join(dir, "odd?name.db"))
	assert.Error(t, err)

	// Reopening an existing database keeps its accounts.
	store, err = OpenStore(filepath.Join(dir, "accounts.db"))
	require.NoError(t, err)
	_, err = store.CreateUser("alice")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = OpenStore(filepath.Join(dir, "accounts.db"))
	require.NoError(t, err)
	defer store.Close()
	_, err = store.UserByName("alice")
	assert.NoError(t, err)
}

func TestUsernames(t *testing.T) {
	store := testStore(t)
	for _, name := range []string{"alice", "Bob.Smith", "r2-d2", "a_b", "x"} {
		_, err := store.CreateUser(name)
		assert.NoError(t, err, name)
	}
	for _, name := range []string{"", "alice@example.org", "-lead", ".hidden", "with space", "ünïcode", string(make([]byte, 65))} {
		_, err := store.CreateUser(name)
		assert.Error(t, err, name)
	}
	_, err := store.CreateUser("ALICE")
	assert.ErrorIs(t, err, ErrUsernameTaken, "usernames are case-insensitive")
	a, err := store.UserByName("Alice")
	require.NoError(t, err)
	assert.Equal(t, "alice", a.Username)
	assert.Len(t, a.Handle, 64)
}

func TestAccountCRUD(t *testing.T) {
	store := testStore(t)
	alice, err := store.CreateUser("alice")
	require.NoError(t, err)
	_, err = store.CreateUser("bob")
	require.NoError(t, err)

	require.NoError(t, store.RenameUser("alice", "carol"))
	_, err = store.UserByName("alice")
	assert.ErrorIs(t, err, ErrNoAccount)
	carol, err := store.UserByName("carol")
	require.NoError(t, err)
	assert.Equal(t, alice.Handle, carol.Handle, "renaming keeps the passkey user handle")
	assert.ErrorIs(t, store.RenameUser("carol", "bob"), ErrUsernameTaken)
	assert.Error(t, store.RenameUser("carol", "bad name"))
	assert.ErrorIs(t, store.RenameUser("nobody", "dave"), ErrNoAccount)

	session, err := store.CreateSession(carol.ID, time.Hour)
	require.NoError(t, err)
	token, err := store.CreateEnrollment("carol", time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.SetDisabled("carol", true))
	_, err = store.SessionUser(session, time.Hour)
	assert.ErrorIs(t, err, ErrInvalidToken, "disabling ends sessions")
	_, err = store.EnrollmentUser(token)
	assert.ErrorIs(t, err, ErrInvalidToken, "disabling voids enrollment links")
	_, err = store.CreateEnrollment("carol", time.Hour)
	assert.Error(t, err)
	require.NoError(t, store.SetDisabled("carol", false))
	assert.ErrorIs(t, store.SetDisabled("nobody", true), ErrNoAccount)

	users, err := store.ListUsers()
	require.NoError(t, err)
	require.Len(t, users, 2)
	assert.Equal(t, "bob", users[0].Username)
	assert.Equal(t, "carol", users[1].Username)

	cred := &webauthn.Credential{ID: []byte{1, 2, 3}, PublicKey: []byte{4}}
	token, err = store.CreateEnrollment("carol", time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.CompleteEnrollment(hashToken(token), carol.ID, cred))
	_, err = store.CreateSession(carol.ID, time.Hour)
	require.NoError(t, err)
	users, err = store.ListUsers()
	require.NoError(t, err)
	assert.Equal(t, 1, users[1].Passkeys)
	assert.Equal(t, 1, users[1].Sessions)

	require.NoError(t, store.DeleteUser("carol"))
	assert.ErrorIs(t, store.DeleteUser("carol"), ErrNoAccount)
	for _, table := range []string{"credentials", "sessions", "enrollments"} {
		var n int
		require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&n))
		assert.Zero(t, n, "%s cascade", table)
	}
}

func TestCredentials(t *testing.T) {
	store := testStore(t)
	a, err := store.CreateUser("alice")
	require.NoError(t, err)
	for _, id := range [][]byte{{0xaa, 1}, {0xaa, 2}, {0x10}} {
		token, err := store.CreateEnrollment("alice", time.Hour)
		require.NoError(t, err)
		require.NoError(t, store.CompleteEnrollment(hashToken(token), a.ID, &webauthn.Credential{ID: id}))
	}
	loaded, err := store.UserByHandle(a.Handle)
	require.NoError(t, err)
	require.Len(t, loaded.Credentials, 3)

	cred := loaded.Credentials[0]
	cred.Authenticator.SignCount = 9
	require.NoError(t, store.UpdateCredential(a.ID, &cred))
	creds, err := store.Credentials(a.ID)
	require.NoError(t, err)
	assert.Equal(t, uint32(9), creds[0].Authenticator.SignCount)
	other, err := store.CreateUser("bob")
	require.NoError(t, err)
	assert.ErrorIs(t, store.UpdateCredential(other.ID, &cred), ErrNoAccount, "credentials are bound to their owner")

	assert.ErrorContains(t, store.DeleteCredential("alice", "qg"), "ambiguous") // both 0xaa IDs start "qg"
	assert.ErrorContains(t, store.DeleteCredential("alice", ""), "no passkey")
	assert.ErrorContains(t, store.DeleteCredential("alice", "zzz"), "no passkey")
	require.NoError(t, store.DeleteCredential("alice", credentialID([]byte{0x10})))
	creds, err = store.Credentials(a.ID)
	require.NoError(t, err)
	assert.Len(t, creds, 2)
	assert.ErrorIs(t, store.DeleteCredential("nobody", "qg"), ErrNoAccount)
}

func TestEnrollmentTokens(t *testing.T) {
	store := testStore(t)
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }
	a, err := store.CreateUser("alice")
	require.NoError(t, err)
	token, err := store.CreateEnrollment("alice", time.Hour)
	require.NoError(t, err)
	var stored []byte
	require.NoError(t, store.db.QueryRow(`SELECT token_hash FROM enrollments`).Scan(&stored))
	assert.NotContains(t, string(stored), token, "only the token hash is stored")

	got, err := store.EnrollmentUser(token)
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.ID)
	_, err = store.EnrollmentUser("wrong")
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.ErrorIs(t, store.CompleteEnrollment(hashToken(token), a.ID+1, &webauthn.Credential{ID: []byte{1}}), ErrInvalidToken, "token is bound to its account")

	now = now.Add(time.Hour + time.Second)
	_, err = store.EnrollmentUser(token)
	assert.ErrorIs(t, err, ErrInvalidToken, "expired")
	assert.ErrorIs(t, store.CompleteEnrollment(hashToken(token), a.ID, &webauthn.Credential{ID: []byte{1}}), ErrInvalidToken)
	_, err = store.CreateEnrollment("nobody", time.Hour)
	assert.ErrorIs(t, err, ErrNoAccount)
}

func TestSessions(t *testing.T) {
	store := testStore(t)
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }
	a, err := store.CreateUser("alice")
	require.NoError(t, err)
	token, err := store.CreateSession(a.ID, 30*24*time.Hour)
	require.NoError(t, err)
	expires := func() int64 {
		var v int64
		require.NoError(t, store.db.QueryRow(`SELECT expires_at FROM sessions WHERE token_hash = ?`, hashToken(token)).Scan(&v))
		return v
	}
	initial := expires()

	now = now.Add(time.Hour)
	_, err = store.SessionUser(token, 30*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, initial, expires(), "not renewed within a day")

	now = now.Add(2 * 24 * time.Hour)
	got, err := store.SessionUser(token, 30*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, "alice", got.Username)
	assert.Equal(t, now.Add(30*24*time.Hour).Unix(), expires(), "sliding renewal")

	now = now.Add(31 * 24 * time.Hour)
	_, err = store.SessionUser(token, 30*24*time.Hour)
	assert.ErrorIs(t, err, ErrInvalidToken, "expired")
	require.NoError(t, store.PurgeExpired())
	var n int
	require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n))
	assert.Zero(t, n)

	now = time.Unix(1_700_000_000, 0)
	token, err = store.CreateSession(a.ID, time.Hour)
	require.NoError(t, err)
	_, err = store.CreateSession(a.ID, time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.DeleteSession(token))
	_, err = store.SessionUser(token, time.Hour)
	assert.ErrorIs(t, err, ErrInvalidToken)
	ended, err := store.DeleteSessions("alice")
	require.NoError(t, err)
	assert.Equal(t, int64(1), ended)
}
