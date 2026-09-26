// Package passkeyauth adds privacy-friendly passkey (WebAuthn) sign-in to a
// net/http app: discoverable credentials, server-side sessions in SQLite,
// accounts created only from the CLI via single-use enrollment links, and a
// generic 404 page for signed-out visitors that reveals nothing about the app.
package passkeyauth

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/chacha20poly1305"
)

//go:embed assets
var assets embed.FS

const (
	ceremonyTTL      = 5 * time.Minute
	maxCeremonyToken = 16 << 10
	maxPasskeyName   = 128
	maxAuthBody      = 64 << 10

	// All routes owned by this package live under /auth/.
	enrollPath = "/auth/enroll"
	apiPrefix  = "/auth/api/"
)

var (
	errSignIn     = errors.New("passkey sign-in failed")
	errEnrollLink = errors.New("this enrollment link is invalid, used or expired")
)

// Config configures an Authenticator.
type Config struct {
	// AppName is shown on the enrollment page, only to holders of a valid
	// link, and stored in passkey managers as the relying party name.
	AppName string
	// Origin is the public URL browsers use, e.g. https://app.example.org or
	// http://localhost:8080. Passkeys are bound to its host name.
	Origin string
	// SessionTTL is the sliding session lifetime. Default 30 days.
	SessionTTL time.Duration
	// CookieName defaults to "__Host-session". Keep the __Host- prefix.
	CookieName string
	// APIPrefix marks the app's API routes, which get a JSON 401 instead of
	// the signed-out page. Default "/api/".
	APIPrefix string
	// IsAPI overrides APIPrefix for apps whose API does not share a prefix.
	// It reports whether a signed-out request gets a JSON 401 rather than
	// the signed-out page.
	IsAPI func(*http.Request) bool
}

// Authenticator provides passkey sign-in with server-side sessions.
type Authenticator struct {
	cfg        Config
	origin     *url.URL
	store      *Store
	webauthn   *webauthn.WebAuthn
	ceremonies *ceremonySealer
	signedOut  []byte
	static     http.Handler
}

// ceremony is the WebAuthn state carried from begin to finish.
type ceremony struct {
	Session   webauthn.SessionData `json:"s"`
	UserID    int64                `json:"u,omitempty"`
	TokenHash []byte               `json:"t,omitempty"` // enrollment only
	Expires   time.Time            `json:"e"`
}

// ceremonySealer turns in-flight WebAuthn state into an opaque token that
// the client carries from begin to finish, so a signed-out client creates no
// server state. A store of pending challenges, however it is bounded, can be
// filled by anyone with enough source addresses, crowding out every real
// sign-in; with nothing stored there is nothing to fill. Tokens are
// XChaCha20-Poly1305 sealed under a per-process key, so they cannot be read or forged, and they
// expire with the ceremony. Single use is tracked only for ceremonies that
// pass verification, which takes a real passkey, so that set grows only with
// genuine sign-ins. Enrollment needs no entry: its link is single use.
type ceremonySealer struct {
	aead cipher.AEAD
	mu   sync.Mutex
	used map[string]time.Time
	now  func() time.Time
}

var ceremonyAAD = []byte("passkeyauth ceremony v1")

func newCeremonySealer(now func() time.Time) (*ceremonySealer, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// XChaCha20-Poly1305's 192-bit random nonces never collide in practice,
	// however many unauthenticated begins one process key seals; AES-GCM's
	// 96-bit ones would need rotating.
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	return &ceremonySealer{aead: aead, used: map[string]time.Time{}, now: now}, nil
}

func (c *ceremonySealer) seal(v ceremony) (string, error) {
	v.Expires = c.now().Add(ceremonyTTL)
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, plain, ceremonyAAD)), nil
}

// open returns the ceremony sealed in token if it is authentic and unexpired.
func (c *ceremonySealer) open(token string) (ceremony, bool) {
	if len(token) > maxCeremonyToken {
		return ceremony{}, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	n := c.aead.NonceSize()
	if err != nil || len(raw) < n {
		return ceremony{}, false
	}
	plain, err := c.aead.Open(nil, raw[:n], raw[n:], ceremonyAAD)
	if err != nil {
		return ceremony{}, false
	}
	var v ceremony
	if err := json.Unmarshal(plain, &v); err != nil || v.Session.Challenge == "" {
		return ceremony{}, false
	}
	return v, c.now().Before(v.Expires)
}

// spend marks a verified ceremony used and reports whether it was fresh.
// It keys on the challenge inside the sealed payload, never on the token's
// spelling, so a re-encoded token cannot be replayed.
func (c *ceremonySealer) spend(v ceremony) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for challenge, expires := range c.used {
		if now.After(expires) {
			delete(c.used, challenge)
		}
	}
	if _, spent := c.used[v.Session.Challenge]; spent {
		return false
	}
	c.used[v.Session.Challenge] = v.Expires
	return true
}

// ValidateOrigin checks the public origin browsers use to reach the app and
// returns the WebAuthn relying party ID derived from it.
func ValidateOrigin(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("invalid origin %q: use scheme://host[:port]", origin)
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("origin %q: passkeys need a host name, not an IP address (try http://localhost:PORT)", origin)
	}
	local := host == "localhost" || strings.HasSuffix(host, ".localhost")
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return "", fmt.Errorf("origin %q: passkeys require https except on localhost", origin)
	}
	return host, nil
}

// EnrollmentURL is the link printed by the CLI for an enrollment token. The
// token travels in the fragment, which browsers never send to servers.
func EnrollmentURL(origin, token string) string {
	return strings.TrimSuffix(origin, "/") + enrollPath + "#token=" + token
}

// New returns an Authenticator for the accounts in store.
func New(store *Store, cfg Config) (*Authenticator, error) {
	rpID, err := ValidateOrigin(cfg.Origin)
	if err != nil {
		return nil, err
	}
	if cfg.AppName == "" {
		return nil, errors.New("passkeyauth: AppName is required")
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "__Host-session"
	}
	if cfg.APIPrefix == "" {
		cfg.APIPrefix = "/api/"
	}
	if cfg.IsAPI == nil {
		prefix := cfg.APIPrefix
		cfg.IsAPI = func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, prefix) }
	}
	// Browsers serialise origins in lower case; a mixed-case RP ID would
	// never match the authenticator's rpIdHash.
	cfg.Origin = strings.ToLower(strings.TrimSuffix(cfg.Origin, "/"))
	timeout := webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTTL, TimeoutUVD: ceremonyTTL}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  rpID,
		RPDisplayName:         cfg.AppName,
		RPOrigins:             []string{cfg.Origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{Login: timeout, Registration: timeout},
	})
	if err != nil {
		return nil, err
	}
	web, err := fs.Sub(assets, "assets")
	if err != nil {
		return nil, err
	}
	page, err := fs.ReadFile(web, "signed-out.html")
	if err != nil {
		return nil, err
	}
	ceremonies, err := newCeremonySealer(time.Now)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(cfg.Origin)
	return &Authenticator{
		cfg: cfg, origin: u, store: store, webauthn: wa, signedOut: page,
		ceremonies: ceremonies,
		static:     http.StripPrefix("/auth/", http.FileServerFS(web)),
	}, nil
}

// Register adds the sign-in, enrollment and session routes under /auth/.
func (a *Authenticator) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+apiPrefix+"login/begin", a.loginBegin)
	mux.HandleFunc("POST "+apiPrefix+"login/finish", a.loginFinish)
	mux.HandleFunc("POST "+apiPrefix+"enroll/check", a.enrollCheck)
	mux.HandleFunc("POST "+apiPrefix+"enroll/begin", a.enrollBegin)
	mux.HandleFunc("POST "+apiPrefix+"enroll/finish", a.enrollFinish)
	mux.HandleFunc("POST "+apiPrefix+"logout", a.logout)
	mux.HandleFunc("GET "+apiPrefix+"me", a.me)
	mux.HandleFunc("GET "+enrollPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/auth/enroll.html"
		a.static.ServeHTTP(w, r2)
	})
	for _, name := range []string{"auth.js", "auth.css", "session.js"} {
		mux.Handle("GET /auth/"+name, a.static)
	}
	// The package owns /auth/. Anything else there, including a public path
	// with the wrong method, must not fall through to an app catch-all that
	// could serve the app shell to a signed-out visitor.
	mux.Handle("/auth/", http.NotFoundHandler())
}

// Protect wraps the app: cross-site POSTs are refused, loopback-IP page loads
// are redirected to a localhost origin, and requests without a session get a
// JSON 401 (API) or the generic signed-out 404 page (anything else).
func (a *Authenticator) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
			return
		}
		if a.loopbackAlias(r) {
			http.Redirect(w, r, a.cfg.Origin+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		if a.public(r) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.Account(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, apiPrefix) || a.cfg.IsAPI(r) {
			signInRequired(w)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(a.signedOut)
	})
}

// public lists what a signed-out browser may load: the enrollment page, the
// sign-in assets and the ceremony endpoints. Nothing here names the app.
// An escaped path is never public: the wrapped handler may route on the raw
// form, so /auth%2Fapi%2Flogin%2Fbegin must not borrow /auth/api/login/begin's
// exemption on its way to somewhere else.
func (a *Authenticator) public(r *http.Request) bool {
	if r.URL.RawPath != "" {
		return false
	}
	switch r.URL.Path {
	case enrollPath, "/auth/auth.js", "/auth/auth.css",
		apiPrefix + "login/begin", apiPrefix + "login/finish", apiPrefix + "logout",
		apiPrefix + "enroll/check", apiPrefix + "enroll/begin", apiPrefix + "enroll/finish":
		return true
	}
	return false
}

// Account returns the signed-in account for r, if any.
func (a *Authenticator) Account(r *http.Request) (Account, bool) {
	c, err := r.Cookie(a.cfg.CookieName)
	if err != nil || c.Value == "" {
		return Account{}, false
	}
	acct, err := a.store.SessionUser(c.Value, a.cfg.SessionTTL)
	if err != nil {
		if !errors.Is(err, ErrInvalidToken) {
			log.Printf("session lookup: %v", err)
		}
		return Account{}, false
	}
	return acct, true
}

// loopbackAlias reports a page load through a loopback IP when the origin is
// localhost on the same port. Passkeys are bound to the origin's host name, so
// such visits are sent to the origin instead of failing at sign-in. It never
// fires behind a proxy, so it cannot loop.
func (a *Authenticator) loopbackAlias(r *http.Request) bool {
	if r.Method != http.MethodGet || a.origin.Hostname() != "localhost" {
		return false
	}
	host, port, err := net.SplitHostPort(r.Host)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback() && port == a.origin.Port()
}

func (a *Authenticator) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: a.cfg.CookieName, Value: token, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (a *Authenticator) startSession(w http.ResponseWriter, acct Account) error {
	token, err := a.store.CreateSession(acct.ID, a.cfg.SessionTTL)
	if err != nil {
		return err
	}
	a.setSessionCookie(w, token, int(a.cfg.SessionTTL/time.Second))
	writeJSON(w, http.StatusOK, map[string]string{"username": acct.Username})
	return nil
}

type ceremonyRequest struct {
	Ceremony   string          `json:"ceremony"`
	Credential json.RawMessage `json:"credential"`
}

func authFail(w http.ResponseWriter, status int, public error, detail error) {
	if detail != nil {
		log.Printf("%v: %v", public, detail)
	}
	writeJSON(w, status, map[string]string{"error": public.Error()})
}

func (a *Authenticator) loginBegin(w http.ResponseWriter, r *http.Request) {
	options, session, err := a.webauthn.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		authFail(w, http.StatusInternalServerError, errSignIn, err)
		return
	}
	id, err := a.ceremonies.seal(ceremony{Session: *session})
	if err != nil {
		authFail(w, http.StatusInternalServerError, errSignIn, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": options})
}

func (a *Authenticator) loginFinish(w http.ResponseWriter, r *http.Request) {
	var req ceremonyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		authFail(w, http.StatusBadRequest, errSignIn, err)
		return
	}
	c, ok := a.ceremonies.open(req.Ceremony)
	if !ok || c.TokenHash != nil {
		authFail(w, http.StatusBadRequest, errors.New("sign-in expired; try again"), nil)
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(req.Credential)
	if err != nil {
		authFail(w, http.StatusBadRequest, errSignIn, err)
		return
	}
	var acct Account
	lookup := func(_, handle []byte) (webauthn.User, error) {
		var err error
		if acct, err = a.store.UserByHandle(handle); err != nil {
			return nil, err
		}
		if acct.Disabled {
			return nil, errors.New("account is disabled")
		}
		return passkeyUser{Account: acct}, nil
	}
	_, cred, err := a.webauthn.ValidatePasskeyLogin(lookup, c.Session, parsed)
	if err != nil {
		authFail(w, http.StatusUnauthorized, errSignIn, err)
		return
	}
	if cred.Authenticator.CloneWarning {
		authFail(w, http.StatusUnauthorized, errSignIn, fmt.Errorf("signature counter did not increase for passkey %s", credentialID(cred.ID)))
		return
	}
	if !a.ceremonies.spend(c) {
		authFail(w, http.StatusBadRequest, errors.New("sign-in expired; try again"), nil)
		return
	}
	if err := a.store.UpdateCredential(acct.ID, cred); err != nil {
		authFail(w, http.StatusInternalServerError, errSignIn, err)
		return
	}
	if err := a.startSession(w, acct); err != nil {
		authFail(w, http.StatusInternalServerError, errSignIn, err)
	}
}

// enrollCheck tells the enrollment page whether its link is valid. The app is
// named only to holders of a valid link.
func (a *Authenticator) enrollCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		authFail(w, http.StatusBadRequest, errEnrollLink, nil)
		return
	}
	acct, err := a.store.EnrollmentUser(req.Token)
	if err != nil {
		authFail(w, http.StatusBadRequest, errEnrollLink, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": acct.Username, "app": a.cfg.AppName})
}

func (a *Authenticator) enrollBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		authFail(w, http.StatusBadRequest, errEnrollLink, nil)
		return
	}
	name := strings.TrimSpace(req.Name)
	if utf8.RuneCountInString(name) > maxPasskeyName || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		authFail(w, http.StatusBadRequest, fmt.Errorf("passkey label must be at most %d printable characters", maxPasskeyName), nil)
		return
	}
	acct, err := a.store.EnrollmentUser(req.Token)
	if err != nil {
		authFail(w, http.StatusBadRequest, errEnrollLink, nil)
		return
	}
	creds, err := a.store.Credentials(acct.ID)
	if err != nil {
		authFail(w, http.StatusInternalServerError, errors.New("enrollment failed"), err)
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range creds {
		acct.Credentials = append(acct.Credentials, c.Credential)
		exclude = append(exclude, c.Descriptor())
	}
	// name travels to the authenticator in the options below and is dropped
	// afterwards; only the random user handle identifies the account.
	options, session, err := a.webauthn.BeginRegistration(passkeyUser{Account: acct, name: name},
		webauthn.WithExclusions(exclude),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		authFail(w, http.StatusInternalServerError, errors.New("enrollment failed"), err)
		return
	}
	id, err := a.ceremonies.seal(ceremony{Session: *session, UserID: acct.ID, TokenHash: hashToken(req.Token)})
	if err != nil {
		authFail(w, http.StatusInternalServerError, errors.New("enrollment failed"), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": options})
}

func (a *Authenticator) enrollFinish(w http.ResponseWriter, r *http.Request) {
	enrollErr := errors.New("passkey enrollment failed")
	var req ceremonyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		authFail(w, http.StatusBadRequest, enrollErr, err)
		return
	}
	c, ok := a.ceremonies.open(req.Ceremony)
	if !ok || c.TokenHash == nil {
		authFail(w, http.StatusBadRequest, errors.New("enrollment expired; reload the page and try again"), nil)
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		authFail(w, http.StatusBadRequest, enrollErr, err)
		return
	}
	acct, err := a.store.user("id = ?", c.UserID)
	if err != nil {
		authFail(w, http.StatusBadRequest, enrollErr, err)
		return
	}
	cred, err := a.webauthn.CreateCredential(passkeyUser{Account: acct}, c.Session, parsed)
	if err != nil {
		authFail(w, http.StatusBadRequest, enrollErr, err)
		return
	}
	// go-webauthn does not enforce excludeCredentials. A re-used ID would
	// only fail the insert and leave the link unspent, so refuse it here.
	existing, err := a.store.Credentials(acct.ID)
	if err != nil {
		authFail(w, http.StatusInternalServerError, enrollErr, err)
		return
	}
	for _, e := range existing {
		if bytes.Equal(e.ID, cred.ID) {
			authFail(w, http.StatusBadRequest, errors.New("this passkey is already registered"), nil)
			return
		}
	}
	// No spend here: CompleteEnrollment consumes the link in the same
	// transaction as the insert, so the link itself is the single-use guard.
	if err := a.store.CompleteEnrollment(c.TokenHash, acct.ID, cred); err != nil {
		authFail(w, http.StatusBadRequest, enrollErr, err)
		return
	}
	if err := a.startSession(w, acct); err != nil {
		authFail(w, http.StatusInternalServerError, enrollErr, err)
	}
}

func (a *Authenticator) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(a.cfg.CookieName); err == nil {
		if err := a.store.DeleteSession(c.Value); err != nil {
			log.Printf("logout: %v", err)
		}
	}
	a.setSessionCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Authenticator) me(w http.ResponseWriter, r *http.Request) {
	acct, ok := a.Account(r)
	if !ok {
		signInRequired(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": acct.Username})
}

// PurgeLoop removes expired sessions and enrollment links hourly until done
// closes.
func (a *Authenticator) PurgeLoop(done <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := a.store.PurgeExpired(); err != nil {
			log.Printf("purge expired sessions: %v", err)
		}
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

// signInRequiredHeader marks a 401 as coming from the session guard, so
// session.js reacts to a lost session and not to an app route's own 401.
const signInRequiredHeader = "X-Passkey-Auth"

func signInRequired(w http.ResponseWriter) {
	w.Header().Set(signInRequiredHeader, "sign-in")
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign-in required"})
}
