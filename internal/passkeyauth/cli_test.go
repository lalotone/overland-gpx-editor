package passkeyauth

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cliHarness struct{ db string }

func (h cliHarness) run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Command(append(args, "--db", h.db), strings.NewReader(stdin), &out, CommandOptions{Program: "myapp user", DefaultDB: h.db, DefaultOrigin: "http://localhost:8080"})
	return out.String(), err
}

func (h cliHarness) ok(t *testing.T, args ...string) string {
	t.Helper()
	out, err := h.run(t, "", args...)
	require.NoError(t, err, out)
	return out
}

var enrollLink = regexp.MustCompile(`(https?://\S+)/auth/enroll#token=([A-Za-z0-9_-]{43})`)

func TestUserCLI(t *testing.T) {
	h := cliHarness{db: filepath.Join(t.TempDir(), "accounts.db")}

	assert.Contains(t, h.ok(t, "list"), "No accounts")
	out := h.ok(t, "add", "alice", "--origin", "https://maps.example.org", "--ttl", "2h")
	assert.Contains(t, out, "Created alice.")
	assert.Contains(t, out, "within 2h0m0s")
	m := enrollLink.FindStringSubmatch(out)
	require.NotNil(t, m, out)
	assert.Equal(t, "https://maps.example.org", m[1])

	store, err := OpenStore(h.db)
	require.NoError(t, err)
	acct, err := store.EnrollmentUser(m[2])
	require.NoError(t, err)
	assert.Equal(t, "alice", acct.Username)
	require.NoError(t, store.Close())

	h.ok(t, "add", "bob")
	out = h.ok(t, "list")
	assert.Regexp(t, `(?m)^alice\s+active\s+0\s+0\s`, out)
	assert.Regexp(t, `(?m)^bob\s+active`, out)

	out = h.ok(t, "show", "alice")
	assert.Contains(t, out, "Username: alice")
	assert.Contains(t, out, "Passkeys: 0")

	assert.Contains(t, h.ok(t, "update", "alice", "--disable"), "alice is now disabled")
	assert.Regexp(t, `(?m)^alice\s+disabled`, h.ok(t, "list"))
	_, err = h.run(t, "", "enroll", "alice")
	assert.ErrorContains(t, err, "disabled")
	assert.Contains(t, h.ok(t, "update", "alice", "--enable", "--rename", "carol"), "Renamed alice to carol.")
	assert.Regexp(t, enrollLink, h.ok(t, "enroll", "carol"))
	assert.Contains(t, h.ok(t, "logout", "carol"), "Ended 0 session(s)")

	_, err = h.run(t, "nope\n", "delete", "carol")
	assert.ErrorContains(t, err, "not deleted")
	out, err = h.run(t, "carol\n", "delete", "carol")
	require.NoError(t, err)
	assert.Contains(t, out, "Deleted carol.")
	assert.Contains(t, h.ok(t, "delete", "bob", "--yes"), "Deleted bob.")
	assert.Contains(t, h.ok(t, "list"), "No accounts")
}

func TestUserCLIErrors(t *testing.T) {
	h := cliHarness{db: filepath.Join(t.TempDir(), "accounts.db")}
	h.ok(t, "add", "alice")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"frobnicate"}, "unknown command"},
		{"missing argument", []string{"show"}, "expects 1 argument"},
		{"extra argument", []string{"list", "x"}, "expects 0 argument"},
		{"unknown flag", []string{"list", "--bogus"}, "flag provided but not defined"},
		{"duplicate", []string{"add", "ALICE"}, "already exists"},
		{"email username", []string{"add", "a@example.org"}, "invalid username"},
		{"plain http origin", []string{"add", "bob", "--origin", "http://maps.example.org"}, "https"},
		{"ip origin", []string{"enroll", "alice", "--origin", "http://127.0.0.1:8080"}, "host name"},
		{"bad ttl", []string{"enroll", "alice", "--ttl", "0s"}, "--ttl"},
		{"long ttl", []string{"enroll", "alice", "--ttl", "2000h"}, "--ttl"},
		{"conflicting update", []string{"update", "alice", "--disable", "--enable"}, "either"},
		{"empty update", []string{"update", "alice"}, "nothing to update"},
		{"no such user", []string{"show", "bob"}, "no such account"},
		{"no such passkey", []string{"revoke", "alice", "abc"}, "no passkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.run(t, "", tt.args...)
			assert.ErrorContains(t, err, tt.want)
		})
	}
	out, err := h.run(t, "", "help")
	require.NoError(t, err)
	assert.Contains(t, out, "Usage: myapp user")
}
