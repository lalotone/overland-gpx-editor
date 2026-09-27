package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/identity"
	"github.com/lalotone/overland-gpx-editor/internal/passkeyauth"
	"github.com/lalotone/overland-gpx-editor/internal/server"
)

const testAuthOrigin = "http://localhost:8000"

type authStack struct {
	handler http.Handler
	store   *passkeyauth.Store
	server  *server.Server
}

// newAuthStack is the real backend behind the passkey guard, with a stand-in
// frontend whose shell names the app.
func newAuthStack(t *testing.T) *authStack {
	t.Helper()
	srv, err := server.New(server.Config{
		DataDir:       t.TempDir(),
		RequireOwner:  true,
		ElevationHost: "http://elevation.invalid",
		// serve does the same: the auth origin is the UI origin.
		TrustedUIOrigin: testAuthOrigin,
		Assets: fstest.MapFS{
			"index.html":        {Data: []byte("<html><head></head><title>GPX Editor</title></html>")},
			"assets/index-1.js": {Data: []byte("console.log('GPX Editor')")},
			"favicon.svg":       {Data: []byte("<svg/>")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	store, err := passkeyauth.OpenStore(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	secret, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "owner.key"))
	if err != nil {
		t.Fatal(err)
	}
	handler, _, err := protectWithPasskeys(srv, store, testAuthOrigin, newOwnerResolver(secret, []string{"Operator"}))
	if err != nil {
		t.Fatal(err)
	}
	return &authStack{handler: handler, store: store, server: srv}
}

func (s *authStack) session(t *testing.T) string {
	return s.sessionFor(t, "rider")
}

func (s *authStack) sessionFor(t *testing.T, name string) string {
	t.Helper()
	acct, err := s.store.CreateUser(name)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.store.CreateSession(acct.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (s *authStack) do(method, target, session string, header map[string]string) *httptest.ResponseRecorder {
	return s.send(method, target, session, header, nil)
}

func (s *authStack) send(method, target, session string, header map[string]string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	req.Host = "localhost:8000"
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "__Host-session", Value: session})
	}
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, req)
	return w
}

func TestSignedOutRequestsNeverReachTheApp(t *testing.T) {
	stack := newAuthStack(t)
	navigate := map[string]string{"Sec-Fetch-Mode": "navigate", "Accept": "text/html"}
	fetch := map[string]string{"Sec-Fetch-Mode": "cors", "Accept": "*/*"}
	for _, tt := range []struct {
		method, target string
		header         map[string]string
		status         int
	}{
		{"GET", "/", navigate, http.StatusNotFound},
		{"GET", "/index.html", navigate, http.StatusNotFound},
		{"GET", "/planner", navigate, http.StatusNotFound},
		{"GET", "/assets/index-1.js", map[string]string{"Sec-Fetch-Mode": "no-cors"}, http.StatusUnauthorized},
		{"GET", "/favicon.svg", nil, http.StatusUnauthorized},
		{"GET", "/files", fetch, http.StatusUnauthorized},
		{"GET", "/files", nil, http.StatusUnauthorized},
		{"GET", "/%66iles", nil, http.StatusUnauthorized},
		{"GET", "/config", fetch, http.StatusUnauthorized},
		{"GET", "/gpx/route.gpx", fetch, http.StatusUnauthorized},
		{"PUT", "/gpx/route.gpx", fetch, http.StatusUnauthorized},
		{"DELETE", "/gpx/route.gpx", fetch, http.StatusUnauthorized},
		{"POST", "/upload", fetch, http.StatusUnauthorized},
		{"GET", "/offline/status", fetch, http.StatusUnauthorized},
		{"GET", "/healthz?x=1", navigate, http.StatusNoContent},
		{"GET", "/%68ealthz", nil, http.StatusUnauthorized},
		{"POST", "/healthz", nil, http.StatusUnauthorized},
		{"GET", "/auth%2Fenroll", navigate, http.StatusNotFound},
		{"POST", "/auth/enroll", nil, http.StatusNotFound},
		{"GET", "/auth/api/logout", navigate, http.StatusNotFound},
	} {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			w := stack.do(tt.method, tt.target, "", tt.header)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "GPX Editor") {
				t.Fatalf("signed-out response leaked the app: %s", w.Body.String())
			}
			if w.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatalf("missing frame protection")
			}
		})
	}
}

func TestSignedInRequestsReachTheApp(t *testing.T) {
	stack := newAuthStack(t)
	session := stack.session(t)
	if w := stack.do("GET", "/", session, map[string]string{"Sec-Fetch-Mode": "navigate"}); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "GPX Editor") {
		t.Fatalf("index = %d %q", w.Code, w.Body.String())
	}
	if w := stack.do("GET", "/files", session, nil); w.Code != http.StatusOK {
		t.Fatalf("files = %d %q", w.Code, w.Body.String())
	}
	if w := stack.do("GET", "/files", "forged", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged session = %d", w.Code)
	}
	// Cross-site writes stay refused even with a session.
	if w := stack.do("POST", "/upload", session, map[string]string{"Origin": "https://evil.example"}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site upload = %d", w.Code)
	}
}

// Each account gets its own library. The wrapper attaches an opaque owner,
// so one user's track is another user's 404, and the backend built with
// RequireOwner refuses the same routes when reached without the wrapper.
func TestEachAccountHasItsOwnLibrary(t *testing.T) {
	stack := newAuthStack(t)
	alice, bob := stack.sessionFor(t, "alice"), stack.sessionFor(t, "bob")
	same := map[string]string{"Origin": testAuthOrigin, "Content-Type": "application/xml"}
	if w := stack.send("PUT", "/gpx/trip.gpx", alice, same, strings.NewReader("<gpx/>")); w.Code != http.StatusOK {
		t.Fatalf("alice save = %d %s", w.Code, w.Body)
	}
	if w := stack.do("GET", "/files", alice, nil); !strings.Contains(w.Body.String(), "trip.gpx") {
		t.Fatalf("alice files = %s", w.Body)
	}
	if w := stack.do("GET", "/files", bob, nil); strings.Contains(w.Body.String(), "trip.gpx") {
		t.Fatalf("bob sees alice's track: %s", w.Body)
	}
	if w := stack.do("GET", "/gpx/trip.gpx", bob, nil); w.Code != http.StatusNotFound {
		t.Fatalf("bob read alice's track = %d", w.Code)
	}
	if w := stack.do("DELETE", "/gpx/trip.gpx", bob, map[string]string{"Origin": testAuthOrigin}); w.Code != http.StatusNotFound {
		t.Fatalf("bob delete alice's track = %d", w.Code)
	}
	if w := stack.do("GET", "/gpx/trip.gpx", alice, nil); w.Code != http.StatusOK {
		t.Fatalf("alice's track after bob's attempts = %d", w.Code)
	}
	// Bypassing the wrapper never yields the local library.
	req := httptest.NewRequest("GET", "/files", nil)
	w := httptest.NewRecorder()
	stack.server.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unwrapped owner route = %d %s", w.Code, w.Body)
	}
	if w := stack.do("GET", "/healthz", "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("healthz = %d", w.Code)
	}
}

func TestOnlyOperatorsChangeServerWideState(t *testing.T) {
	stack := newAuthStack(t)
	rider, operator := stack.sessionFor(t, "rider"), stack.sessionFor(t, "operator")
	header := map[string]string{"Origin": testAuthOrigin, "Content-Type": "application/json", "X-GPX-Editor": "1"}
	body := func() io.Reader { return strings.NewReader(`{"mode":"cache-only"}`) }
	if w := stack.send("PUT", "/offline/mode", rider, header, body()); w.Code != http.StatusForbidden {
		t.Fatalf("rider changed the offline mode: %d %s", w.Code, w.Body)
	}
	// The match is case-insensitive, as the account database's is.
	if w := stack.send("PUT", "/offline/mode", operator, header, body()); w.Code != http.StatusOK {
		t.Fatalf("operator = %d %s", w.Code, w.Body)
	}
	// Ordinary users keep their own offline data.
	if w := stack.do("GET", "/offline/packs", rider, map[string]string{"Sec-Fetch-Site": "same-origin"}); w.Code != http.StatusOK {
		t.Fatalf("rider packs = %d %s", w.Code, w.Body)
	}
}

func TestOwnerResolverCachesSessionsAndForgetsRevokedOnes(t *testing.T) {
	stack := newAuthStack(t)
	session := stack.session(t)
	if w := stack.do("GET", "/files", session, nil); w.Code != http.StatusOK {
		t.Fatalf("first = %d", w.Code)
	}
	if w := stack.do("GET", "/files", session, nil); w.Code != http.StatusOK {
		t.Fatalf("cached = %d", w.Code)
	}
	if _, err := stack.store.DeleteSessions("rider"); err != nil {
		t.Fatal(err)
	}
	if w := stack.do("GET", "/files", session, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session still served: %d", w.Code)
	}
}

func TestHealthCheckOnLoopbackIPIsNotRedirected(t *testing.T) {
	stack := newAuthStack(t)
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Host = "127.0.0.1:8000"
	w := httptest.NewRecorder()
	stack.handler.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("healthz = %d", w.Code)
	}
}

func TestAPIRequest(t *testing.T) {
	for _, tt := range []struct {
		mode, accept string
		api          bool
	}{
		{"navigate", "text/html", false},
		{"navigate", "", false},
		{"cors", "text/html", true},
		{"no-cors", "", true},
		{"", "text/html,application/xhtml+xml", false},
		{"", "application/json", true},
		{"", "", true},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		if tt.mode != "" {
			req.Header.Set("Sec-Fetch-Mode", tt.mode)
		}
		if tt.accept != "" {
			req.Header.Set("Accept", tt.accept)
		}
		if got := apiRequest(req); got != tt.api {
			t.Errorf("apiRequest(mode=%q, accept=%q) = %v, want %v", tt.mode, tt.accept, got, tt.api)
		}
	}
}

func TestAuthOrigin(t *testing.T) {
	for _, tt := range []struct {
		explicit, addr, want string
		fails                bool
	}{
		{"", "127.0.0.1:8000", "http://localhost:8000", false},
		{"", ":9000", "http://localhost:9000", false},
		{"", "127.0.0.1:0", "", true},
		{"https://maps.example.org/", "127.0.0.1:0", "https://maps.example.org", false},
		{"http://maps.example.org", "127.0.0.1:8000", "", true},
		{"https://192.168.1.10", "127.0.0.1:8000", "", true},
	} {
		got, err := authOrigin(tt.explicit, tt.addr)
		if (err != nil) != tt.fails || got != tt.want {
			t.Errorf("authOrigin(%q, %q) = %q, %v", tt.explicit, tt.addr, got, err)
		}
	}
}

func TestAuthWarnings(t *testing.T) {
	if got := authWarnings("http://localhost:8000", "127.0.0.1:8000", nil, ""); len(got) != 0 {
		t.Fatalf("local default warned: %v", got)
	}
	if got := authWarnings("https://maps.example.org", "127.0.0.1:8000", []string{"https://maps.example.org/"}, "https://Maps.example.org"); len(got) != 0 {
		t.Fatalf("matching proxy origin warned: %v", got)
	}
	for _, tt := range []struct {
		addr, trusted string
		allowed       []string
	}{
		{"0.0.0.0:8000", "", nil},
		{":8000", "", nil},
		{"127.0.0.1:8000", "", []string{"https://ui.example.org"}},
		{"127.0.0.1:8000", "https://ui.example.org", nil},
	} {
		if got := authWarnings("http://localhost:8000", tt.addr, tt.allowed, tt.trusted); len(got) == 0 {
			t.Errorf("no warning for addr=%q allowed=%v trusted=%q", tt.addr, tt.allowed, tt.trusted)
		}
	}
}

func TestListenAddr(t *testing.T) {
	for _, tt := range []struct {
		addr    string
		addrSet bool
		port    int
		want    string
		fails   bool
	}{
		{"127.0.0.1:8000", false, 0, "127.0.0.1:8000", false},
		{"0.0.0.0:9000", true, 0, "0.0.0.0:9000", false},
		{"127.0.0.1:8000", false, 8950, "127.0.0.1:8950", false},
		{"0.0.0.0:9000", true, 8950, "", true},
		{"127.0.0.1:8000", false, 70000, "", true},
		{"127.0.0.1:8000", false, -1, "", true},
	} {
		got, err := listenAddr(tt.addr, tt.addrSet, tt.port)
		if (err != nil) != tt.fails || got != tt.want {
			t.Errorf("listenAddr(%q, %v, %d) = %q, %v", tt.addr, tt.addrSet, tt.port, got, err)
		}
	}
}
