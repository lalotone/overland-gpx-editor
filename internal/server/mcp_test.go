package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	overlandmcp "github.com/lalotone/overland-gpx-editor/internal/mcp"
)

func TestDisabledMCPWithFrontend(t *testing.T) {
	srv, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid",
		Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html><head></head><body>app</body>")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	for _, path := range []string{"/mcp", "/mcp/", "/mcp/browser/session", "/mcp/browser/events", "/mcp/browser/view"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	for _, path := range []string{"/", "/planner", "/mcp-guide"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("SPA path %s status = %d, want 200", path, rec.Code)
		}
	}
}

// The agent endpoint lives on its own loopback listener, so the main router —
// the one a reverse proxy fronts — must expose the browser bridge and nothing
// else under /mcp.
func TestMainRouterServesOnlyTheBrowserBridge(t *testing.T) {
	wantPath := ""
	marker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			t.Errorf("bridge path = %q, want %q", r.URL.Path, wantPath)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	enabled, err := New(Config{
		GPXDir:            t.TempDir(),
		ElevationHost:     "http://elevation.invalid",
		MCPBrowserHandler: marker,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { enabled.Close() })

	wantPath = "/session"
	recorder := httptest.NewRecorder()
	enabled.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/mcp/browser/session", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("bridge route status = %d", recorder.Code)
	}

	// Chi answers 405 rather than 404 for a path sitting above a mount. Either
	// is fine; what matters is that nothing under /mcp reaches a handler.
	for _, path := range []string{"/mcp", "/mcp/", "/mcp/not-real"} {
		recorder := httptest.NewRecorder()
		enabled.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 404 or 405", path, recorder.Code)
		}
	}

	disabled, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disabled.Close() })
	recorder = httptest.NewRecorder()
	disabled.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/mcp/browser/session", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("disabled MCP route status = %d, want 404", recorder.Code)
	}
}

func TestBrowserBridgeRejectsNonLoopbackPeer(t *testing.T) {
	bridge, err := overlandmcp.NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	srv, err := New(Config{
		GPXDir:            t.TempDir(),
		ElevationHost:     "http://elevation.invalid",
		MCPBrowserHandler: bridge,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	request := httptest.NewRequest(http.MethodGet, "/mcp/browser/session", nil)
	request.Host = "127.0.0.1:8000"
	request.RemoteAddr = "192.0.2.10:32000"
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote bridge status = %d, want 403", recorder.Code)
	}
}

func TestSkipPathExemptsOnlyTheCommandStream(t *testing.T) {
	applied := 0
	counting := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			applied++
			next.ServeHTTP(w, r)
		})
	}
	handler := skipPath(mcpBrowserEventsPath, counting)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// A long-lived stream must never occupy a throttle slot, but everything
	// else — including near-miss paths — still has to be throttled.
	for _, test := range []struct {
		path string
		want int
	}{
		{path: mcpBrowserEventsPath, want: 0},
		{path: mcpBrowserEventsPath + "/", want: 1},
		{path: "/mcp/browser/view", want: 2},
		{path: "/files", want: 3},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d", test.path, recorder.Code)
		}
		if applied != test.want {
			t.Errorf("%s middleware applications = %d, want %d", test.path, applied, test.want)
		}
	}
}
