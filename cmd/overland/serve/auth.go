package serve

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/lalotone/overland-gpx-editor/internal/passkeyauth"
)

// authAppName is shown on the enrollment page and stored in passkey managers
// as the relying party name. Signed-out pages never show it.
const authAppName = "Overland"

// protectWithPasskeys puts passkey sign-in in front of the whole app. It
// wraps the finished handler rather than living in internal/server, so the
// mobile host, which embeds that package behind its own capability cookie,
// neither changes behaviour nor links the WebAuthn and SQLite dependencies.
func protectWithPasskeys(app http.Handler, store *passkeyauth.Store, origin string) (http.Handler, *passkeyauth.Authenticator, error) {
	auth, err := passkeyauth.New(store, passkeyauth.Config{
		AppName: authAppName,
		Origin:  origin,
		IsAPI:   apiRequest,
	})
	if err != nil {
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/", app)
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
