package passkeyauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testOrigin = "http://localhost:8080"

// softKey is a minimal software authenticator holding one ES256 passkey.
type softKey struct {
	key    *ecdsa.PrivateKey
	id     []byte
	handle []byte
	count  uint32
	origin string
	// synced keeps the signature counter at 0, as synced passkeys do, so the
	// clone check cannot catch a replay.
	synced bool
}

func newSoftKey(t *testing.T) *softKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &softKey{key: key, id: id, origin: testOrigin}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	require.NoError(t, err)
	return b
}

func (k *softKey) clientData(typ, challenge string) []byte {
	data, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return data
}

func (k *softKey) authData(rpID string, flags byte, attested []byte) []byte {
	hash := sha256.Sum256([]byte(rpID))
	out := append(hash[:], flags)
	out = binary.BigEndian.AppendUint32(out, k.count)
	return append(out, attested...)
}

// create answers navigator.credentials.create options from enroll/begin.
func (k *softKey) create(t *testing.T, options map[string]any) json.RawMessage {
	t.Helper()
	pk := options["publicKey"].(map[string]any)
	k.handle = unb64(t, pk["user"].(map[string]any)["id"].(string))
	rpID := pk["rp"].(map[string]any)["id"].(string)
	point, err := k.key.PublicKey.Bytes() // 0x04 || X || Y
	require.NoError(t, err)
	x, y := point[1:33], point[33:]
	cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	require.NoError(t, err)
	attested := append(make([]byte, 16), byte(len(k.id)>>8), byte(len(k.id)))
	attested = append(append(attested, k.id...), cose...)
	auth := k.authData(rpID, 0x01|0x04|0x40, attested)
	obj, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": auth})
	require.NoError(t, err)
	out, _ := json.Marshal(map[string]any{
		"id": b64(k.id), "rawId": b64(k.id), "type": "public-key", "clientExtensionResults": map[string]any{},
		"response": map[string]any{"clientDataJSON": b64(k.clientData("webauthn.create", pk["challenge"].(string))), "attestationObject": b64(obj)},
	})
	return out
}

// get answers navigator.credentials.get options from login/begin.
func (k *softKey) get(t *testing.T, options map[string]any) json.RawMessage {
	t.Helper()
	pk := options["publicKey"].(map[string]any)
	if !k.synced {
		k.count++
	}
	auth := k.authData(pk["rpId"].(string), 0x01|0x04, nil)
	client := k.clientData("webauthn.get", pk["challenge"].(string))
	digest := sha256.Sum256(append(slicesClone(auth), sha256Sum(client)...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	require.NoError(t, err)
	out, _ := json.Marshal(map[string]any{
		"id": b64(k.id), "rawId": b64(k.id), "type": "public-key", "clientExtensionResults": map[string]any{},
		"response": map[string]any{"clientDataJSON": b64(client), "authenticatorData": b64(auth), "signature": b64(sig), "userHandle": b64(k.handle)},
	})
	return out
}

func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }
func sha256Sum(b []byte) []byte   { h := sha256.Sum256(b); return h[:] }

type authFixture struct {
	store   *Store
	auth    *Authenticator
	handler http.Handler
	dir     string
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "accounts.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	auth, err := New(store, Config{AppName: "Test App", Origin: testOrigin})
	require.NoError(t, err)
	// A stand-in app: one page and one API route behind the guard.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<title>Test App</title>")) })
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("// app")) })
	mux.HandleFunc("POST /api/order", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	auth.Register(mux)
	return &authFixture{store: store, auth: auth, handler: auth.Protect(mux), dir: dir}
}

func (f *authFixture) do(t *testing.T, method, path string, body any, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "__Host-session", Value: cookie})
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func decodeMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m), w.Body.String())
	return m
}

func sessionFrom(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-session" {
			assert.True(t, c.Secure)
			assert.True(t, c.HttpOnly)
			assert.Equal(t, http.SameSiteStrictMode, c.SameSite)
			assert.Equal(t, "/", c.Path)
			return c.Value
		}
	}
	require.Fail(t, "no session cookie", w.Body.String())
	return ""
}

func (f *authFixture) enroll(t *testing.T, key *softKey, token, name string) *httptest.ResponseRecorder {
	t.Helper()
	w := f.do(t, "POST", "/auth/api/enroll/begin", map[string]string{"token": token, "name": name}, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	begin := decodeMap(t, w)
	cred := key.create(t, begin["options"].(map[string]any))
	return f.do(t, "POST", "/auth/api/enroll/finish", map[string]any{"ceremony": begin["ceremony"], "credential": cred}, "")
}

func (f *authFixture) login(t *testing.T, key *softKey) *httptest.ResponseRecorder {
	t.Helper()
	w := f.do(t, "POST", "/auth/api/login/begin", nil, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	begin := decodeMap(t, w)
	cred := key.get(t, begin["options"].(map[string]any))
	return f.do(t, "POST", "/auth/api/login/finish", map[string]any{"ceremony": begin["ceremony"], "credential": cred}, "")
}

func (f *authFixture) newUser(t *testing.T, name string) (*softKey, string) {
	t.Helper()
	_, err := f.store.CreateUser(name)
	require.NoError(t, err)
	key := newSoftKey(t)
	token, err := f.store.CreateEnrollment(name, time.Hour)
	require.NoError(t, err)
	w := f.enroll(t, key, token, "alice@example.org")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return key, sessionFrom(t, w)
}

func TestPasskeyWorkflow(t *testing.T) {
	f := newAuthFixture(t)

	// Signed-out visitors get the same generic 404 page everywhere.
	for _, path := range []string{"/", "/app.js", "/login.html", "/no/such/page"} {
		w := f.do(t, "GET", path, nil, "")
		assert.Equal(t, http.StatusNotFound, w.Code, path)
		assert.Contains(t, w.Body.String(), `id="passkey"`, path)
		assert.NotContains(t, w.Body.String(), "Test App", path)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), path)
	}
	assert.Equal(t, http.StatusUnauthorized, f.do(t, "POST", "/api/order", map[string]any{}, "").Code)
	assert.Equal(t, http.StatusUnauthorized, f.do(t, "GET", "/auth/api/me", nil, "").Code)
	for _, path := range []string{"/auth/enroll", "/auth/auth.js", "/auth/auth.css"} {
		w := f.do(t, "GET", path, nil, "")
		assert.Equal(t, http.StatusOK, w.Code, path)
		assert.NotContains(t, w.Body.String(), "Test App", path)
	}

	_, err := f.store.CreateUser("alice")
	require.NoError(t, err)
	token, err := f.store.CreateEnrollment("alice", time.Hour)
	require.NoError(t, err)
	w := f.do(t, "POST", "/auth/api/enroll/check", map[string]string{"token": token}, "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{"username": "alice", "app": "Test App"}, decodeMap(t, w))
	w = f.do(t, "POST", "/auth/api/enroll/check", map[string]string{"token": "bogus"}, "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.NotContains(t, w.Body.String(), "Test App")
	key := newSoftKey(t)
	w = f.enroll(t, key, token, "alice@example.org")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "alice", decodeMap(t, w)["username"])
	session := sessionFrom(t, w)

	w = f.do(t, "GET", "/auth/api/me", nil, session)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "alice", decodeMap(t, w)["username"])
	w = f.do(t, "GET", "/", nil, session)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Test App")

	// The enrollment link is single use.
	assert.Equal(t, http.StatusBadRequest, f.do(t, "POST", "/auth/api/enroll/check", map[string]string{"token": token}, "").Code)
	w = f.do(t, "POST", "/auth/api/enroll/begin", map[string]string{"token": token}, "")
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = f.do(t, "POST", "/auth/api/logout", nil, session)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, http.StatusUnauthorized, f.do(t, "GET", "/auth/api/me", nil, session).Code)

	w = f.login(t, key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "alice", decodeMap(t, w)["username"])
	session = sessionFrom(t, w)
	assert.Equal(t, http.StatusOK, f.do(t, "GET", "/auth/api/me", nil, session).Code)

	acct, err := f.store.UserByHandle(key.handle)
	require.NoError(t, err)
	require.Len(t, acct.Credentials, 1)
	assert.Equal(t, uint32(1), acct.Credentials[0].Authenticator.SignCount)
}

// The name a user gives their passkey (often an email) goes to the
// authenticator only and must never reach the database files.
func TestPasskeyNameIsNotStored(t *testing.T) {
	f := newAuthFixture(t)
	f.newUser(t, "alice")
	require.NoError(t, f.store.Close())
	files, err := filepath.Glob(filepath.Join(f.dir, "accounts.db*"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, path := range files {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "alice@example.org", path)
		assert.NotContains(t, string(data), "example.org", path)
	}
}

func TestPasskeyLoginRejections(t *testing.T) {
	t.Run("disabled account", func(t *testing.T) {
		f := newAuthFixture(t)
		key, session := f.newUser(t, "alice")
		require.NoError(t, f.store.SetDisabled("alice", true))
		assert.Equal(t, http.StatusUnauthorized, f.do(t, "GET", "/auth/api/me", nil, session).Code)
		assert.Equal(t, http.StatusUnauthorized, f.login(t, key).Code)
		require.NoError(t, f.store.SetDisabled("alice", false))
		assert.Equal(t, http.StatusOK, f.login(t, key).Code)
	})
	t.Run("revoked passkey", func(t *testing.T) {
		f := newAuthFixture(t)
		key, _ := f.newUser(t, "alice")
		require.NoError(t, f.store.DeleteCredential("alice", b64(key.id)[:6]))
		assert.Equal(t, http.StatusUnauthorized, f.login(t, key).Code)
	})
	t.Run("deleted account", func(t *testing.T) {
		f := newAuthFixture(t)
		key, session := f.newUser(t, "alice")
		require.NoError(t, f.store.DeleteUser("alice"))
		assert.Equal(t, http.StatusUnauthorized, f.do(t, "GET", "/auth/api/me", nil, session).Code)
		assert.Equal(t, http.StatusUnauthorized, f.login(t, key).Code)
	})
	t.Run("cloned authenticator", func(t *testing.T) {
		f := newAuthFixture(t)
		key, _ := f.newUser(t, "alice")
		require.Equal(t, http.StatusOK, f.login(t, key).Code)
		key.count = 0 // next assertion reuses counter 1
		assert.Equal(t, http.StatusUnauthorized, f.login(t, key).Code)
	})
	t.Run("wrong origin", func(t *testing.T) {
		f := newAuthFixture(t)
		key, _ := f.newUser(t, "alice")
		key.origin = "http://localhost:9999"
		assert.Equal(t, http.StatusUnauthorized, f.login(t, key).Code)
	})
	t.Run("unknown passkey", func(t *testing.T) {
		f := newAuthFixture(t)
		f.newUser(t, "alice")
		stranger := newSoftKey(t)
		stranger.handle = make([]byte, 64)
		assert.Equal(t, http.StatusUnauthorized, f.login(t, stranger).Code)
	})
	t.Run("ceremony is single use", func(t *testing.T) {
		f := newAuthFixture(t)
		key, _ := f.newUser(t, "alice")
		begin := decodeMap(t, f.do(t, "POST", "/auth/api/login/begin", nil, ""))
		body := map[string]any{"ceremony": begin["ceremony"], "credential": key.get(t, begin["options"].(map[string]any))}
		assert.Equal(t, http.StatusOK, f.do(t, "POST", "/auth/api/login/finish", body, "").Code)
		assert.NotEqual(t, http.StatusOK, f.do(t, "POST", "/auth/api/login/finish", body, "").Code)
	})
	t.Run("synced passkey cannot replay a sign-in", func(t *testing.T) {
		f := newAuthFixture(t)
		key, _ := f.newUser(t, "alice")
		key.synced = true
		key.count = 0
		begin := decodeMap(t, f.do(t, "POST", "/auth/api/login/begin", nil, ""))
		body := map[string]any{"ceremony": begin["ceremony"], "credential": key.get(t, begin["options"].(map[string]any))}
		assert.Equal(t, http.StatusOK, f.do(t, "POST", "/auth/api/login/finish", body, "").Code)
		w := f.do(t, "POST", "/auth/api/login/finish", body, "")
		assert.Equal(t, http.StatusBadRequest, w.Code, "the spent ceremony, not the counter, refuses it")
		assert.Empty(t, w.Result().Cookies())
	})
	t.Run("enrollment ceremony cannot finish a login", func(t *testing.T) {
		f := newAuthFixture(t)
		key, _ := f.newUser(t, "alice")
		token, err := f.store.CreateEnrollment("alice", time.Hour)
		require.NoError(t, err)
		begin := decodeMap(t, f.do(t, "POST", "/auth/api/enroll/begin", map[string]string{"token": token}, ""))
		body := map[string]any{"ceremony": begin["ceremony"], "credential": key.get(t, map[string]any{"publicKey": map[string]any{"rpId": "localhost", "challenge": "x"}})}
		assert.Equal(t, http.StatusBadRequest, f.do(t, "POST", "/auth/api/login/finish", body, "").Code)
	})
}

func TestEnrollmentRejections(t *testing.T) {
	f := newAuthFixture(t)
	_, err := f.store.CreateUser("alice")
	require.NoError(t, err)
	token, err := f.store.CreateEnrollment("alice", time.Hour)
	require.NoError(t, err)

	for _, body := range []map[string]string{
		{"token": "bogus"},
		{"token": token, "name": strings.Repeat("x", maxPasskeyName+1)},
		{"token": token, "name": "bad\nname"},
	} {
		assert.Equal(t, http.StatusBadRequest, f.do(t, "POST", "/auth/api/enroll/begin", body, "").Code, body)
	}

	// Two ceremonies for the same link: only the first to finish succeeds.
	first := decodeMap(t, f.do(t, "POST", "/auth/api/enroll/begin", map[string]string{"token": token}, ""))
	second := decodeMap(t, f.do(t, "POST", "/auth/api/enroll/begin", map[string]string{"token": token}, ""))
	a, b := newSoftKey(t), newSoftKey(t)
	w := f.do(t, "POST", "/auth/api/enroll/finish", map[string]any{"ceremony": first["ceremony"], "credential": a.create(t, first["options"].(map[string]any))}, "")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = f.do(t, "POST", "/auth/api/enroll/finish", map[string]any{"ceremony": second["ceremony"], "credential": b.create(t, second["options"].(map[string]any))}, "")
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// A second passkey needs a new link and excludes the first one.
	token, err = f.store.CreateEnrollment("alice", time.Hour)
	require.NoError(t, err)
	begin := decodeMap(t, f.do(t, "POST", "/auth/api/enroll/begin", map[string]string{"token": token}, ""))
	exclude := begin["options"].(map[string]any)["publicKey"].(map[string]any)["excludeCredentials"].([]any)
	require.Len(t, exclude, 1)
	assert.Equal(t, b64(a.id), exclude[0].(map[string]any)["id"])

	// A disabled account can't redeem its link.
	require.NoError(t, f.store.SetDisabled("alice", true))
	assert.Equal(t, http.StatusBadRequest, f.do(t, "POST", "/auth/api/enroll/begin", map[string]string{"token": token}, "").Code)
}

func TestLoopbackAliasRedirect(t *testing.T) {
	f := newAuthFixture(t)
	tests := []struct {
		method, host string
		redirect     bool
	}{
		{"GET", "127.0.0.1:8080", true},
		{"GET", "[::1]:8080", true},
		{"GET", "127.0.0.1:9090", false},
		{"GET", "localhost:8080", false},
		{"GET", "192.168.1.5:8080", false},
		{"POST", "127.0.0.1:8080", false},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.host, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/auth/enroll?x=1", nil)
			req.Host = tt.host
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, req)
			if tt.redirect {
				assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
				assert.Equal(t, testOrigin+"/auth/enroll?x=1", w.Header().Get("Location"))
				return
			}
			assert.NotEqual(t, http.StatusTemporaryRedirect, w.Code)
		})
	}
}

func TestAuthRejectsCrossOrigin(t *testing.T) {
	f := newAuthFixture(t)
	req := httptest.NewRequest("POST", "/auth/api/login/begin", nil)
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestPasskeyOrigin(t *testing.T) {
	tests := []struct {
		origin, rpID string
		wantErr      bool
	}{
		{"http://localhost:8080", "localhost", false},
		{"https://maps.example.org", "maps.example.org", false},
		{"https://maps.example.org/", "maps.example.org", false},
		{"http://gpx.localhost:9000", "gpx.localhost", false},
		{"http://maps.example.org", "", true},
		{"http://127.0.0.1:8080", "", true},
		{"https://[::1]:8080", "", true},
		{"https://example.org/app", "", true},
		{"localhost:8080", "", true},
		{"https://user@example.org", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.origin, func(t *testing.T) {
			rpID, err := ValidateOrigin(tt.origin)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.rpID, rpID)
		})
	}
}

func TestCeremonySealer(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c, err := newCeremonySealer(func() time.Time { return now })
	require.NoError(t, err)
	token, err := c.seal(ceremony{Session: webauthn.SessionData{Challenge: "abc"}, UserID: 7})
	require.NoError(t, err)
	got, ok := c.open(token)
	require.True(t, ok)
	assert.Equal(t, int64(7), got.UserID)
	assert.NotContains(t, token, "abc", "sealed, not just encoded")

	assert.True(t, c.spend(got))
	assert.False(t, c.spend(got), "single use once verified")

	// Tampering anywhere, including the trailing bits base64 would otherwise
	// ignore, is refused rather than yielding a second spellable token.
	raw, err := base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 1
	_, ok = c.open(base64.RawURLEncoding.EncodeToString(raw))
	assert.False(t, ok, "tampered")
	last := token[len(token)-1]
	for _, alt := range "AEIMQUYcgkosw048" {
		if byte(alt) != last {
			_, ok = c.open(token[:len(token)-1] + string(alt))
			assert.False(t, ok, "non-canonical spelling %q", string(alt))
		}
	}
	other, err := newCeremonySealer(func() time.Time { return now })
	require.NoError(t, err)
	_, ok = other.open(token)
	assert.False(t, ok, "another process's key")
	_, ok = c.open(strings.Repeat("A", maxCeremonyToken+1))
	assert.False(t, ok, "oversized")

	now = now.Add(ceremonyTTL + time.Second)
	_, ok = c.open(token)
	assert.False(t, ok, "expired")
	fresh, err := c.seal(ceremony{Session: webauthn.SessionData{Challenge: "def"}})
	require.NoError(t, err)
	v, _ := c.open(fresh)
	c.spend(v)
	assert.Len(t, c.used, 1, "expired spends are pruned")
}

func TestBeginCreatesNoServerState(t *testing.T) {
	f := newAuthFixture(t)
	for range 5000 {
		require.Equal(t, http.StatusOK, f.do(t, "POST", "/auth/api/login/begin", nil, "").Code)
	}
	assert.Empty(t, f.auth.ceremonies.used)
	// A real sign-in still completes after the flood.
	key, _ := f.newUser(t, "alice")
	w := f.login(t, key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Len(t, f.auth.ceremonies.used, 1, "only verified sign-ins are tracked; enrollment spends its link")
}

func TestGuard401IsMarked(t *testing.T) {
	f := newAuthFixture(t)
	for _, path := range []string{"/api/order", "/auth/api/me"} {
		method := "GET"
		if path == "/api/order" {
			method = "POST"
		}
		w := f.do(t, method, path, nil, "")
		assert.Equal(t, http.StatusUnauthorized, w.Code, path)
		assert.Equal(t, "sign-in", w.Header().Get("X-Passkey-Auth"), path)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), path)
	}
}

func TestMixedCaseOriginIsNormalised(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "accounts.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	auth, err := New(store, Config{AppName: "Test App", Origin: "https://Maps.Example.ORG/"})
	require.NoError(t, err)
	assert.Equal(t, "https://maps.example.org", auth.cfg.Origin)
	assert.Equal(t, "maps.example.org", auth.webauthn.Config.RPID)
}

func TestEscapedPathIsNotPublic(t *testing.T) {
	f := newAuthFixture(t)
	// The same route spelled with an escaped separator must not inherit the
	// public exemption: the wrapped app may route on the raw form.
	w := f.do(t, "POST", "/auth%2Fapi%2Flogin%2Fbegin", nil, "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	w = f.do(t, "GET", "/auth%2Fauth.js", nil, "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "Nothing here")
}

func TestIsAPIOverride(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "accounts.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	auth, err := New(store, Config{AppName: "Test App", Origin: testOrigin, IsAPI: func(r *http.Request) bool {
		return r.Header.Get("Sec-Fetch-Mode") != "navigate"
	}})
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /files", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("secret")) })
	auth.Register(mux)
	handler := auth.Protect(mux)

	req := httptest.NewRequest("GET", "/files", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.NotContains(t, w.Body.String(), "secret")

	req = httptest.NewRequest("GET", "/files", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "Nothing here")
	assert.NotContains(t, w.Body.String(), "secret")
}

func TestUnknownAuthPathsNeverReachTheApp(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "accounts.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	auth, err := New(store, Config{AppName: "Test App", Origin: testOrigin})
	require.NoError(t, err)
	mux := http.NewServeMux()
	// An SPA-style catch-all that would happily serve the shell for any path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<title>Test App</title>")) })
	auth.Register(mux)
	handler := auth.Protect(mux)
	for _, tt := range []struct{ method, path string }{
		{"POST", "/auth/enroll"},         // public path, wrong method
		{"GET", "/auth/api/logout"},      // public path, wrong method
		{"GET", "/auth/api/login/begin"}, // public path, wrong method
		{"GET", "/auth/anything"},
	} {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		assert.NotContains(t, w.Body.String(), "Test App", "%s %s", tt.method, tt.path)
	}
}

func TestRevokeEndsSessions(t *testing.T) {
	f := newAuthFixture(t)
	key, session := f.newUser(t, "alice")
	require.Equal(t, http.StatusOK, f.do(t, "GET", "/auth/api/me", nil, session).Code)
	var out bytes.Buffer
	err := Command([]string{"revoke", "alice", b64(key.id)[:8], "--db", filepath.Join(f.dir, "accounts.db")},
		strings.NewReader(""), &out, CommandOptions{Program: "test user", DefaultOrigin: testOrigin})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "ended 1 session(s)")
	assert.Equal(t, http.StatusUnauthorized, f.do(t, "GET", "/auth/api/me", nil, session).Code)
}

func TestEnrollmentRefusesARegisteredPasskey(t *testing.T) {
	f := newAuthFixture(t)
	key, _ := f.newUser(t, "alice")
	token, err := f.store.CreateEnrollment("alice", time.Hour)
	require.NoError(t, err)
	// excludeCredentials is advisory; a client can still send an existing ID.
	for range 3 {
		w := f.enroll(t, key, token, "")
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
	assert.Empty(t, f.auth.ceremonies.used, "refused enrollments leave no state")
	w := f.enroll(t, newSoftKey(t), token, "")
	assert.Equal(t, http.StatusOK, w.Code, "the link still works for a new passkey")
	assert.Equal(t, http.StatusBadRequest, f.do(t, "POST", "/auth/api/enroll/begin", map[string]string{"token": token}, "").Code, "and only once")
}
