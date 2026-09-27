package serve

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/identity"
	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/lalotone/overland-gpx-editor/internal/passkeyauth"
	"github.com/lalotone/overland-gpx-editor/internal/server"
)

// authAppName is shown on the enrollment page and stored in passkey managers
// as the relying party name. Signed-out pages never show it.
const authAppName = "Overland"

// ownerResolver is the only place account details and owner keys meet. It
// runs inside the guard, after Protect has accepted the session, and hands
// the backend an opaque owner and an operator mark: never the account.
type ownerResolver struct {
	secret    identity.Secret
	operators map[string]struct{} // lower-cased usernames from --auth-operator

	// A session token always belongs to the same account, so its owner can
	// be remembered without a second database lookup per tile request. The
	// guard has already refused a revoked or expired token before this runs.
	mu    sync.Mutex
	cache map[[sha256.Size]byte]ownerEntry
}

type ownerEntry struct {
	owner    server.Owner
	operator bool
}

const maxCachedSessions = 4096

func newOwnerResolver(secret identity.Secret, operators []string) *ownerResolver {
	r := &ownerResolver{secret: secret, operators: make(map[string]struct{}, len(operators)), cache: make(map[[sha256.Size]byte]ownerEntry)}
	for _, name := range operators {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			r.operators[name] = struct{}{}
		}
	}
	return r
}

// isOperator matches a username the way the account database does: case
// insensitively.
func (r *ownerResolver) isOperator(username string) bool {
	_, ok := r.operators[strings.ToLower(username)]
	return ok
}

func (r *ownerResolver) resolve(auth *passkeyauth.Authenticator, req *http.Request) (ownerEntry, bool) {
	cookie, err := req.Cookie("__Host-session")
	if err != nil || cookie.Value == "" {
		return ownerEntry{}, false
	}
	key := sha256.Sum256([]byte(cookie.Value))
	r.mu.Lock()
	entry, cached := r.cache[key]
	r.mu.Unlock()
	if cached {
		return entry, true
	}
	acct, ok := auth.Account(req)
	if !ok {
		return ownerEntry{}, false
	}
	owner, err := r.secret.Owner(acct)
	if err != nil {
		return ownerEntry{}, false
	}
	entry = ownerEntry{owner: owner, operator: r.isOperator(acct.Username)}
	r.mu.Lock()
	if len(r.cache) >= maxCachedSessions {
		r.cache = make(map[[sha256.Size]byte]ownerEntry)
	}
	r.cache[key] = entry
	r.mu.Unlock()
	return entry, true
}

// attach marks the request with its owner, and operator status, before the
// app sees it. Protect has already verified the session; a miss here means
// it was revoked in between, which gets the same 401 the guard would give.
func (r *ownerResolver) attach(auth *passkeyauth.Authenticator, app http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		entry, ok := r.resolve(auth, req)
		if !ok {
			w.Header().Set("X-Passkey-Auth", "sign-in")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"sign-in required"}`))
			return
		}
		ctx := server.WithOwner(req.Context(), entry.owner)
		if entry.operator {
			ctx = server.WithOperator(ctx)
		}
		app.ServeHTTP(w, req.WithContext(ctx))
	})
}

// protectWithPasskeys puts passkey sign-in in front of the whole app. It
// wraps the finished handler rather than living in internal/server, so the
// mobile host, which embeds that package behind its own capability cookie,
// neither changes behaviour nor links the WebAuthn and SQLite dependencies.
// The app is built with RequireOwner, so it refuses owner-scoped requests
// that somehow reach it without passing through attach.
func protectWithPasskeys(app http.Handler, store *passkeyauth.Store, origin string, owners *ownerResolver) (http.Handler, *passkeyauth.Authenticator, error) {
	auth, err := passkeyauth.New(store, passkeyauth.Config{
		AppName: authAppName,
		Origin:  origin,
		IsAPI:   apiRequest,
	})
	if err != nil {
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/", owners.attach(auth, app))
	auth.Register(mux)
	protected := auth.Protect(mux)
	return authSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness probes carry no session and usually hit the loopback IP,
		// which Protect would redirect to the localhost origin. Answer them
		// here so the app itself is never reached without a session.
		if healthCheck(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		protected.ServeHTTP(w, r)
	})), auth, nil
}

// apiRequest decides what a signed-out request gets back. The app's API has
// no common prefix, so only a top-level browser navigation receives the
// sign-in page; fetches, tiles, scripts and CLI clients get a JSON 401.
func apiRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Mode") {
	case "navigate":
		return false
	case "":
		return !strings.Contains(r.Header.Get("Accept"), "text/html")
	}
	return true
}

// authWarnings explains configurations that start fine but leave nobody able
// to sign in or use the app. Every one of them fails closed.
func authWarnings(origin, addr string, allowedOrigins []string, trustedUIOrigin string) []string {
	var warnings []string
	// Behind a proxy, naming the auth origin itself is how signed-in users get
	// offline management; only a different origin is a dead end.
	if trusted := strings.TrimSuffix(strings.TrimSpace(trustedUIOrigin), "/"); trusted != "" && !strings.EqualFold(trusted, origin) {
		warnings = append(warnings, "--trusted-ui-origin "+trustedUIOrigin+" is not the --auth origin "+origin+"; a UI there cannot carry the passkey session")
	}
	for _, allowed := range allowedOrigins {
		if !strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(allowed), "/"), origin) {
			warnings = append(warnings, "--allowed-origin "+allowed+" is not the --auth origin "+origin+"; pages there cannot sign in or send the session cookie")
		}
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && strings.HasPrefix(origin, "http://localhost:") {
		if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
			warnings = append(warnings, "--addr "+addr+" listens beyond loopback, but passkeys are bound to "+origin+"; remote browsers cannot sign in without --auth-origin https://…")
		}
	}
	return warnings
}

// operatorWarnings names configured operators that cannot sign in. Without an
// operator nobody can change the offline mode or prepare a routing region
// from the browser; the admin token still can.
func operatorWarnings(store *passkeyauth.Store, operators []string) []string {
	var warnings []string
	if len(operators) == 0 {
		warnings = append(warnings, "no --auth-operator: offline mode, routing regions and the shared cache can only be managed with --offline-admin-token")
	}
	for _, name := range operators {
		acct, err := store.UserByName(strings.TrimSpace(name))
		switch {
		case err != nil:
			warnings = append(warnings, "--auth-operator "+name+" is not an account; create it with `"+util.AppName+" user add`")
		case acct.Disabled:
			warnings = append(warnings, "--auth-operator "+name+" is disabled")
		}
	}
	return warnings
}

// healthCheck matches only the canonical spelling: the app routes on the raw
// path, so an escaped variant must not share the exemption.
func healthCheck(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.URL.Path == "/healthz" && r.URL.RawPath == ""
}

// authSecurityHeaders covers the sign-in and enrollment pages, which are
// served before the app's own header middleware runs. Framing is refused so
// the Sign in button cannot be clickjacked.
func authSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// authOrigin returns the public origin passkeys bind to. Without an explicit
// one it assumes a local browser on the listener's port; a random port would
// make every passkey unusable after a restart, so that is refused.
func authOrigin(explicit, addr string) (string, error) {
	if origin := strings.TrimSpace(explicit); origin != "" {
		if _, err := passkeyauth.ValidateOrigin(origin); err != nil {
			return "", err
		}
		return strings.ToLower(strings.TrimSuffix(origin, "/")), nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || port == "0" {
		return "", fmt.Errorf("--auth needs --auth-origin when --addr %q has no fixed port", addr)
	}
	return "http://localhost:" + port, nil
}
