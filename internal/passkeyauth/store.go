package passkeyauth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	_ "modernc.org/sqlite"
)

// The Account database holds usernames, random WebAuthn user handles, passkey
// public keys and hashed session/enrollment tokens. Emails and display names
// shown by passkey managers are sent to the authenticator only; they are never
// stored here.

var (
	ErrNoAccount     = errors.New("no such account")
	ErrInvalidToken  = errors.New("invalid or expired token")
	usernamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	ErrUsernameTaken = errors.New("username already exists")
)

const accountSchema = `
CREATE TABLE IF NOT EXISTS users (
	id         INTEGER PRIMARY KEY,
	handle     BLOB NOT NULL UNIQUE,
	username   TEXT NOT NULL UNIQUE COLLATE NOCASE,
	disabled   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS credentials (
	id         BLOB PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	data       BLOB NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS credentials_user ON credentials(user_id);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash BLOB PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
CREATE TABLE IF NOT EXISTS enrollments (
	token_hash BLOB PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);
PRAGMA user_version = 1;
`

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type Account struct {
	ID          int64
	Handle      []byte
	Username    string
	Disabled    bool
	Created     time.Time
	Credentials []webauthn.Credential
}

type AccountSummary struct {
	Account
	Passkeys int
	Sessions int
}

type StoredCredential struct {
	webauthn.Credential
	Created time.Time
}

// DefaultStorePath returns <user config dir>/<app>/accounts.db.
func DefaultStorePath(app string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, app, "accounts.db"), nil
}

func OpenStore(path string) (*Store, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("Account database path must not contain '?' or '#'")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=secure_delete(1)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(accountSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("Account database: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func validUsername(name string) error {
	if !usernamePattern.MatchString(name) {
		return fmt.Errorf("invalid username %q: use 1–64 letters, digits, '.', '_' or '-', starting with a letter or digit", name)
	}
	return nil
}

func randomToken() (string, []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func (s *Store) CreateUser(username string) (Account, error) {
	if err := validUsername(username); err != nil {
		return Account{}, err
	}
	handle := make([]byte, 64)
	_, _ = rand.Read(handle)
	now := s.now()
	res, err := s.db.Exec(`INSERT INTO users(handle, username, created_at) VALUES(?, ?, ?)`, handle, username, now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Account{}, ErrUsernameTaken
		}
		return Account{}, err
	}
	id, err := res.LastInsertId()
	return Account{ID: id, Handle: handle, Username: username, Created: time.Unix(now.Unix(), 0)}, err
}

func (s *Store) user(where string, arg any) (Account, error) {
	var a Account
	var created int64
	err := s.db.QueryRow(`SELECT id, handle, username, disabled, created_at FROM users WHERE `+where, arg).
		Scan(&a.ID, &a.Handle, &a.Username, &a.Disabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNoAccount
	}
	a.Created = time.Unix(created, 0)
	return a, err
}

func (s *Store) UserByName(username string) (Account, error) {
	return s.user("username = ?", username)
}

// UserByHandle returns the Account and its passkeys for a WebAuthn user handle.
func (s *Store) UserByHandle(handle []byte) (Account, error) {
	a, err := s.user("handle = ?", handle)
	if err != nil {
		return a, err
	}
	creds, err := s.Credentials(a.ID)
	for _, c := range creds {
		a.Credentials = append(a.Credentials, c.Credential)
	}
	return a, err
}

func (s *Store) ListUsers() ([]AccountSummary, error) {
	rows, err := s.db.Query(`SELECT u.id, u.username, u.disabled, u.created_at,
		(SELECT count(*) FROM credentials c WHERE c.user_id = u.id),
		(SELECT count(*) FROM sessions t WHERE t.user_id = u.id AND t.expires_at > ?)
		FROM users u ORDER BY u.username`, s.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountSummary
	for rows.Next() {
		var a AccountSummary
		var created int64
		if err := rows.Scan(&a.ID, &a.Username, &a.Disabled, &created, &a.Passkeys, &a.Sessions); err != nil {
			return nil, err
		}
		a.Created = time.Unix(created, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) exec(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrUsernameTaken
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoAccount
	}
	return nil
}

func (s *Store) RenameUser(username, newName string) error {
	if err := validUsername(newName); err != nil {
		return err
	}
	return s.exec(`UPDATE users SET username = ? WHERE username = ?`, newName, username)
}

// SetDisabled blocks or restores sign-in. Disabling also ends all sessions and
// voids outstanding enrollment links.
func (s *Store) SetDisabled(username string, disabled bool) error {
	return s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET disabled = ? WHERE username = ?`, disabled, username)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNoAccount
		}
		if !disabled {
			return nil
		}
		for _, table := range []string{"sessions", "enrollments"} {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE user_id = (SELECT id FROM users WHERE username = ?)`, username); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteUser(username string) error {
	return s.exec(`DELETE FROM users WHERE username = ?`, username)
}

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) Credentials(userID int64) ([]StoredCredential, error) {
	rows, err := s.db.Query(`SELECT data, created_at FROM credentials WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredCredential
	for rows.Next() {
		var data []byte
		var created int64
		if err := rows.Scan(&data, &created); err != nil {
			return nil, err
		}
		c := StoredCredential{Created: time.Unix(created, 0)}
		if err := json.Unmarshal(data, &c.Credential); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UpdateCredential(userID int64, cred *webauthn.Credential) error {
	data, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return s.exec(`UPDATE credentials SET data = ? WHERE id = ? AND user_id = ?`, data, cred.ID, userID)
}

// DeleteCredential removes the passkey whose base64url ID starts with prefix.
func (s *Store) DeleteCredential(username, prefix string) error {
	a, err := s.UserByName(username)
	if err != nil {
		return err
	}
	creds, err := s.Credentials(a.ID)
	if err != nil {
		return err
	}
	var match []byte
	for _, c := range creds {
		if prefix != "" && strings.HasPrefix(credentialID(c.ID), prefix) {
			if match != nil {
				return fmt.Errorf("passkey ID prefix %q is ambiguous", prefix)
			}
			match = c.ID
		}
	}
	if match == nil {
		return fmt.Errorf("%s has no passkey matching %q", username, prefix)
	}
	return s.exec(`DELETE FROM credentials WHERE id = ? AND user_id = ?`, match, a.ID)
}

func credentialID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

// CreateEnrollment returns a one-time token that lets its holder register a
// passkey for the account.
func (s *Store) CreateEnrollment(username string, ttl time.Duration) (string, error) {
	a, err := s.UserByName(username)
	if err != nil {
		return "", err
	}
	if a.Disabled {
		return "", fmt.Errorf("%s is disabled", username)
	}
	token, hash := randomToken()
	_, err = s.db.Exec(`INSERT INTO enrollments(token_hash, user_id, expires_at) VALUES(?, ?, ?)`, hash, a.ID, s.now().Add(ttl).Unix())
	return token, err
}

func (s *Store) EnrollmentUser(token string) (Account, error) {
	var id int64
	err := s.db.QueryRow(`SELECT e.user_id FROM enrollments e JOIN users u ON u.id = e.user_id
		WHERE e.token_hash = ? AND e.expires_at > ? AND u.disabled = 0`, hashToken(token), s.now().Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrInvalidToken
	}
	if err != nil {
		return Account{}, err
	}
	return s.user("id = ?", id)
}

// CompleteEnrollment consumes the enrollment token and stores the passkey in
// one transaction, so a token can register at most one passkey.
func (s *Store) CompleteEnrollment(tokenHash []byte, userID int64, cred *webauthn.Credential) error {
	data, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM enrollments WHERE token_hash = ? AND user_id = ? AND expires_at > ?
			AND user_id IN (SELECT id FROM users WHERE disabled = 0)`, tokenHash, userID, s.now().Unix())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrInvalidToken
		}
		_, err = tx.Exec(`INSERT INTO credentials(id, user_id, data, created_at) VALUES(?, ?, ?, ?)`, cred.ID, userID, data, s.now().Unix())
		return err
	})
}

func (s *Store) CreateSession(userID int64, ttl time.Duration) (string, error) {
	token, hash := randomToken()
	_, err := s.db.Exec(`INSERT INTO sessions(token_hash, user_id, expires_at) VALUES(?, ?, ?)`, hash, userID, s.now().Add(ttl).Unix())
	return token, err
}

// SessionUser resolves a session token. Sessions slide: an active session is
// extended to ttl at most once a day.
func (s *Store) SessionUser(token string, ttl time.Duration) (Account, error) {
	hash := hashToken(token)
	var id, expires int64
	now := s.now()
	err := s.db.QueryRow(`SELECT s.user_id, s.expires_at FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ? AND u.disabled = 0`, hash, now.Unix()).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrInvalidToken
	}
	if err != nil {
		return Account{}, err
	}
	if renewed := now.Add(ttl).Unix(); renewed-expires > int64(24*time.Hour/time.Second) {
		if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token_hash = ?`, renewed, hash); err != nil {
			return Account{}, err
		}
	}
	return s.user("id = ?", id)
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

func (s *Store) DeleteSessions(username string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = (SELECT id FROM users WHERE username = ?)`, username)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) PurgeExpired() error {
	now := s.now().Unix()
	for _, table := range []string{"sessions", "enrollments"} {
		if _, err := s.db.Exec(`DELETE FROM `+table+` WHERE expires_at <= ?`, now); err != nil {
			return err
		}
	}
	return nil
}

// passkeyUser adapts an Account to webauthn.User. name is shown by the user's
// passkey manager; it may be an email and is not persisted by the server.
type passkeyUser struct {
	Account
	name string
}

func (u passkeyUser) WebAuthnID() []byte { return u.Handle }
func (u passkeyUser) WebAuthnName() string {
	if u.name != "" {
		return u.name
	}
	return u.Username
}
func (u passkeyUser) WebAuthnDisplayName() string                { return u.WebAuthnName() }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }
