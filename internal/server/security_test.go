package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOfflineManagementSecurity(t *testing.T) {
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
		TrustedUIOrigin: "https://planner.example.test", OfflineAdminToken: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	body := `{"name":"trip","bbox":{"south":40,"west":-1,"north":41,"east":0},"zoomMin":1,"zoomMax":1}`
	tests := []struct {
		name   string
		remote string
		origin string
		editor string
		token  string
		want   int
	}{
		{"remote no origin", "192.0.2.10:1", "", "", "", 403},
		{"loopback cli", "127.0.0.1:1", "", "", "", 200},
		{"trusted missing header", "192.0.2.10:1", "https://planner.example.test", "", "", 403},
		{"trusted browser", "192.0.2.10:1", "https://planner.example.test", "1", "", 200},
		{"evil browser", "192.0.2.10:1", "https://evil.example.test", "1", "", 403},
		{"spoofed host", "192.0.2.10:1", "https://evil.example.test", "1", "", 403},
		{"admin token", "192.0.2.10:1", "", "", "secret", 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/offline/packs/estimate", strings.NewReader(body))
			req.RemoteAddr = tt.remote
			if tt.name == "spoofed host" {
				req.Host = "evil.example.test"
			}
			req.Header.Set("Content-Type", "application/json")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.editor != "" {
				req.Header.Set("X-GPX-Editor", tt.editor)
			}
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestOfflineManagementDoesNotTrustGeneralCORSOrigins(t *testing.T) {
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
		AllowedOrigins: []string{"https://allowed.example.test"}, TrustedUIOrigin: "https://planner.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	req := httptest.NewRequest(http.MethodGet, "/offline/packs", nil)
	req.RemoteAddr = "192.0.2.10:1"
	req.Header.Set("Origin", "https://allowed.example.test")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestOfflineReadWithoutOriginRejectsRemoteHostAndFetchMetadata(t *testing.T) {
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
		TrustedUIOrigin: "https://planner.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	for _, host := range []string{"planner.example.test", "attacker.example.test"} {
		req := httptest.NewRequest(http.MethodGet, "/offline/packs", nil)
		req.RemoteAddr = "192.0.2.10:1"
		req.Host = host
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("host %q status = %d, want %d", host, rec.Code, http.StatusForbidden)
		}
	}
}

func TestOfflineManagementAllowsDefaultLoopbackBrowser(t *testing.T) {
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	req := httptest.NewRequest(http.MethodPost, "/offline/packs/estimate", strings.NewReader(`{"name":"trip","bbox":{"south":40,"west":-1,"north":41,"east":0},"zoomMin":1,"zoomMax":1}`))
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = "localhost:8000"
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("X-GPX-Editor", "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	preflight := httptest.NewRequest(http.MethodOptions, "/offline/packs", nil)
	preflight.RemoteAddr = "127.0.0.1:1"
	preflight.Host = "localhost:8000"
	preflight.Header.Set("Origin", "http://localhost:5173")
	preflight.Header.Set("Access-Control-Request-Headers", "content-type, x-gpx-editor")
	preflightRec := httptest.NewRecorder()
	s.ServeHTTP(preflightRec, preflight)
	if preflightRec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d (%s)", preflightRec.Code, http.StatusNoContent, preflightRec.Body)
	}
}

func TestOfflineManagementRejectsReboundOrForgedDefaultOrigins(t *testing.T) {
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	body := `{"name":"trip","bbox":{"south":40,"west":-1,"north":41,"east":0},"zoomMin":1,"zoomMax":1}`
	tests := []struct {
		name, remote, host, origin string
	}{
		{"dns rebind", "127.0.0.1:1", "attacker.example.test", "https://attacker.example.test"},
		{"forged loopback origin", "192.0.2.10:1", "localhost:8000", "http://localhost:8000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/offline/packs/estimate", strings.NewReader(body))
			req.RemoteAddr = tt.remote
			req.Host = tt.host
			req.Header.Set("Origin", tt.origin)
			req.Header.Set("X-GPX-Editor", "1")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusForbidden, rec.Body)
			}
		})
	}
}

func TestOfflineStatusContainsOnlyPackAggregates(t *testing.T) {
	s := newTestServer(t)
	id := strings.Repeat("a", 32)
	s.packs.mu.Lock()
	s.packs.packs[id] = &packManifest{ID: id, Name: "private trip", State: "running"}
	s.packs.mu.Unlock()
	rec := do(t, s, http.MethodGet, "/offline/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), id) || strings.Contains(rec.Body.String(), "private trip") {
		t.Fatalf("status leaked pack details: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"activeJobs":1`) {
		t.Fatalf("status missing aggregate: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"jobs":[{"id":"active","state":"running"`) {
		t.Fatalf("status missing frontend job aggregate: %s", rec.Body)
	}
}

func TestTrafficGeneratingRoutesRejectRemoteRelayRequests(t *testing.T) {
	var calls atomic.Int64
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errorsNew("unexpected transport")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	tests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/elevation?lat=1&lon=2", ""},
		{http.MethodPost, "/elevation/batch", `{"locations":[{"latitude":1,"longitude":2}]}`},
		{http.MethodPost, "/elevation/prefetch", `{"bbox":[1,2,3,4]}`},
		{http.MethodGet, "/fuel", ""},
		{http.MethodGet, "/places/search?q=test", ""},
		{http.MethodPost, "/pois/search", `{}`},
		{http.MethodGet, "/map/raster/osm/1/0/0.png", ""},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
		req.RemoteAddr = "192.0.2.10:1"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d", tt.method, tt.path, rec.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("relay requests made %d outbound calls", calls.Load())
	}
}

func TestRemoteSameOriginFetchMetadataAllowsLegitimateDataAndMapGets(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		contentType := "application/json"
		body := `[{"place_id":1,"display_name":"Madrid","lat":"40.4","lon":"-3.7"}]`
		if strings.Contains(r.URL.Host, "tile.openstreetmap.org") {
			contentType = "image/png"
			body = "\x89PNG\r\n\x1a\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {contentType}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
		NominatimURL: "https://nominatim.example.test", HTTPClient: client,
		AllowedOrigins: []string{"https://planner.example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	for _, path := range []string{"/places/search?q=Madrid", "/map/raster/osm/1/0/0.png"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "192.0.2.10:1"
		req.Host = "planner.example.test"
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d (%s)", path, rec.Code, rec.Body)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("outbound calls = %d, want 2", calls.Load())
	}
}

func TestOutboundResourcesRejectReboundSameHostOrigin(t *testing.T) {
	var calls atomic.Int64
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errorsNew("unexpected transport")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	req := httptest.NewRequest(http.MethodGet, "/places/search?q=private", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = "attacker.example.test"
	req.Header.Set("Origin", "https://attacker.example.test")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusForbidden, rec.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("rebound request made %d outbound calls", calls.Load())
	}
}

func TestProviderRequestsDoNotForwardInboundCredentials(t *testing.T) {
	var sawCredentials atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "https://github.com/lalotone/overland-gpx-editor" || !strings.Contains(r.Header.Get("User-Agent"), "ops@example.test") {
			sawCredentials.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer upstream.Close()
	s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", NominatimURL: upstream.URL, UpstreamContact: "ops@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	req := httptest.NewRequest(http.MethodGet, "/places/search?q=test", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Authorization", "Bearer inbound")
	req.Header.Set("Cookie", "session=private")
	req.Header.Set("Referer", "https://evil.example")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if sawCredentials.Load() {
		t.Fatal("outbound identity was wrong or inbound credentials were forwarded")
	}
}

func TestRedirectToUnapprovedOriginIsRejected(t *testing.T) {
	targetCalls := atomic.Int64{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirector.Close()
	s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", FuelURL: redirector.URL})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	rec := do(t, s, http.MethodGet, "/fuel", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	if targetCalls.Load() != 0 {
		t.Fatal("redirect reached an unapproved origin")
	}
}

func TestConfigurationValidation(t *testing.T) {
	base := Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid"}
	tests := []Config{
		func() Config { c := base; c.OfflineMode = "sometimes"; return c }(),
		func() Config { c := base; c.OfflineCacheMaxBytes = -1; return c }(),
		func() Config { c := base; c.OfflineCacheMaxEntries = -1; return c }(),
		func() Config {
			c := base
			c.RoutingCacheDir = t.TempDir()
			c.RoutingIndexURL = "file:///etc/passwd"
			return c
		}(),
		func() Config { c := base; c.OverpassURL = "http://user:pass@example.test"; return c }(),
		func() Config { c := base; c.OpenFreeMapURL = "https://example.test/style?token=secret"; return c }(),
		func() Config { c := base; c.TrustedUIOrigin = "https://example.test/path"; return c }(),
	}
	for i, config := range tests {
		if server, err := New(config); err == nil {
			server.Close()
			t.Errorf("invalid config %d accepted", i)
		}
	}
}

// Behind a reverse proxy every request arrives from the proxy's loopback
// address, so trusting the peer would hand offline management and the resource
// relay to any remote client that simply omits an Origin header.
func TestBehindProxyWithdrawsLoopbackTrust(t *testing.T) {
	for _, test := range []struct {
		name        string
		behindProxy bool
		wantStatus  int
	}{
		{name: "direct loopback deployment", behindProxy: false, wantStatus: http.StatusOK},
		{name: "proxied deployment", behindProxy: true, wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, err := New(Config{
				GPXDir:        t.TempDir(),
				ElevationHost: "http://elevation.invalid",
				BehindProxy:   test.behindProxy,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { srv.Close() })

			// A remote client reaching a proxied server looks exactly like
			// this: loopback peer, no Origin, no browser headers.
			request := httptest.NewRequest(http.MethodGet, "/offline/packs", nil)
			request.RemoteAddr = "127.0.0.1:32000"
			recorder := httptest.NewRecorder()
			srv.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("offline management status = %d, want %d", recorder.Code, test.wantStatus)
			}
		})
	}
}

func TestBehindProxyStillHonoursTheAdminToken(t *testing.T) {
	srv, err := New(Config{
		GPXDir:            t.TempDir(),
		ElevationHost:     "http://elevation.invalid",
		BehindProxy:       true,
		OfflineAdminToken: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	request := httptest.NewRequest(http.MethodGet, "/offline/packs", nil)
	request.RemoteAddr = "203.0.113.10:32000"
	request.Header.Set("Authorization", "Bearer s3cret")
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("token-authorized management status = %d, want 200", recorder.Code)
	}
}
