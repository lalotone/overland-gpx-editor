package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDataServicesWarmRestartCacheOnlyScenario(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fuel":
			fmt.Fprint(w, `{"Fecha":"28/08/2026","ListaEESSPrecio":[]}`)
		case "/search":
			fmt.Fprintf(w, `[{"place_id":1,"display_name":%q,"lat":"1","lon":"2"}]`, r.URL.Query().Get("q"))
		default:
			http.NotFound(w, r)
		}
	}))
	cacheDir := t.TempDir()
	config := Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: cacheDir,
		FuelURL: upstream.URL + "/fuel", NominatimURL: upstream.URL,
	}
	online, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, online, http.MethodGet, "/fuel", nil); rec.Code != 200 || rec.Header().Get("X-GPX-Cache") != "miss" {
		t.Fatalf("fuel warm = %d cache=%q", rec.Code, rec.Header().Get("X-GPX-Cache"))
	}
	if rec := do(t, online, http.MethodGet, "/places/search?q=Alpha&language=es", nil); rec.Code != 200 {
		t.Fatalf("place warm = %d %s", rec.Code, rec.Body)
	}
	if err := online.Close(); err != nil {
		t.Fatal(err)
	}
	warmedCalls := calls.Load()
	upstream.Close()

	transportCalls := atomic.Int64{}
	config.OfflineMode = "cache-only"
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		transportCalls.Add(1)
		return nil, errorsNew("offline")
	})}
	offline, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer offline.Close()
	if rec := do(t, offline, http.MethodGet, "/fuel", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"Fecha":"28/08/2026"`) || rec.Header().Get("X-GPX-Cache") != "hit" {
		t.Fatalf("offline fuel = %d cache=%q %s", rec.Code, rec.Header().Get("X-GPX-Cache"), rec.Body)
	}
	if rec := do(t, offline, http.MethodGet, "/places/search?q=Alpha&language=es", nil); rec.Code != 200 {
		t.Fatalf("offline place = %d %s", rec.Code, rec.Body)
	}
	miss := do(t, offline, http.MethodGet, "/places/search?q=Beta&language=es", nil)
	if miss.Code != http.StatusGatewayTimeout {
		t.Fatalf("new search = %d %s", miss.Code, miss.Body)
	}
	var payload map[string]string
	if err := json.Unmarshal(miss.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != "offline_cache_miss" || payload["scope"] != "places" {
		t.Errorf("miss payload = %v", payload)
	}
	if transportCalls.Load() != 0 || calls.Load() != warmedCalls {
		t.Fatalf("cache-only used transport: injected=%d upstream delta=%d", transportCalls.Load(), calls.Load()-warmedCalls)
	}
}

func TestPlaceCacheKeyPreservesTheForwardedQueryCase(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"place_id":1,"display_name":%q,"lat":"1","lon":"2"}]`, r.URL.Query().Get("q"))
	}))
	defer upstream.Close()
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
		NominatimURL: upstream.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	for _, query := range []string{"Alpha", "alpha"} {
		if rec := do(t, s, http.MethodGet, "/places/search?q="+query, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", query, rec.Code, rec.Body)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
}

func TestTerrariumWarmRestartCacheOnlyScenario(t *testing.T) {
	tiles := newTileServer(t)
	cacheDir := t.TempDir()
	online, err := New(Config{GPXDir: t.TempDir(), ElevationTiles: true, ElevationTileURL: tiles.url(), ElevationTileCache: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	warm := do(t, online, http.MethodGet, "/elevation?lat=42&lon=-0.5", nil)
	if warm.Code != 200 {
		t.Fatalf("warm = %d %s", warm.Code, warm.Body)
	}
	if err := online.Close(); err != nil {
		t.Fatal(err)
	}
	warmedCalls := tiles.requests.Load()

	transportCalls := atomic.Int64{}
	offline, err := New(Config{
		GPXDir: t.TempDir(), ElevationTiles: true, ElevationTileURL: tiles.url(), ElevationTileCache: cacheDir,
		OfflineMode: "cache-only", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			transportCalls.Add(1)
			return nil, errorsNew("must remain offline")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer offline.Close()
	if rec := do(t, offline, http.MethodGet, "/elevation?lat=42&lon=-0.5", nil); rec.Code != 200 {
		t.Fatalf("offline tile hit = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, offline, http.MethodGet, "/elevation?lat=-42&lon=100", nil); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("offline tile miss = %d %s", rec.Code, rec.Body)
	}
	prefetch := do(t, offline, http.MethodPost, "/elevation/prefetch", strings.NewReader(`{"bbox":[-43,99,-41,101]}`))
	if prefetch.Code != 200 || !strings.Contains(prefetch.Body.String(), `"skipped":true`) {
		t.Fatalf("offline prefetch = %d %s", prefetch.Code, prefetch.Body)
	}
	if transportCalls.Load() != 0 || tiles.requests.Load() != warmedCalls {
		t.Fatalf("offline Terrarium called transport: %d", transportCalls.Load())
	}
}

func TestNarrowEndpointValidationStopsHostileInput(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errorsNew("unexpected") })}
	s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	tests := []struct{ path, body string }{
		{"/pois/search", `{"kind":"raw","bbox":{"south":1,"west":1,"north":2,"east":2},"url":"http://169.254.169.254"}`},
		{"/pois/search", `{"kind":"fuel","bbox":{"south":0,"west":0,"north":20,"east":20}}`},
		{"/routing/valhalla/route", `{"waypoints":[{"lat":0,"lon":0},{"lat":1,"lon":1}],"costing":"pedestrian"}`},
		{"/routing/valhalla/route", `{"waypoints":[{"lat":91,"lon":0},{"lat":1,"lon":1}],"costing":"auto"}`},
		{"/routing/osrm/route", `{"points":[{"lat":0,"lon":0}],"url":"http://localhost"}`},
		{"/routing/valhalla/surface", `{"points":[{"lat":0,"lon":0},{"lat":80,"lon":80}],"costing":"motorcycle"}`},
	}
	for _, tt := range tests {
		rec := do(t, s, http.MethodPost, tt.path, strings.NewReader(tt.body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d (%s)", tt.path, rec.Code, rec.Body)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests made %d transport calls", calls.Load())
	}
}

func TestProviderResponseLimitAndContentTypeAreEnforced(t *testing.T) {
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`"` + strings.Repeat("x", (2<<20)+1) + `"`))
	}))
	defer large.Close()
	s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", NominatimURL: large.URL})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	if rec := do(t, s, http.MethodGet, "/places/search?q=x", nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("large response = %d", rec.Code)
	}

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html>error</html>`)
	}))
	defer html.Close()
	s2, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OverpassURL: html.URL})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s2)
	rec := do(t, s2, http.MethodPost, "/pois/search", strings.NewReader(`{"kind":"fuel","bbox":{"south":1,"west":1,"north":2,"east":2}}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("HTML response = %d %s", rec.Code, rec.Body)
	}
}

func TestConfigAndStatusExposeCapabilitiesWithoutSecrets(t *testing.T) {
	cacheDir := t.TempDir()
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: cacheDir,
		OfflineMode: "cache-only", OfflineAdminToken: "do-not-expose", UpstreamContact: "private@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	config := do(t, s, http.MethodGet, "/config", nil)
	for _, expected := range []string{`"mode":"cache-only"`, `"fuel":"/fuel"`, `"osm":"/map/raster/osm/{z}/{x}/{y}.png"`} {
		if !strings.Contains(config.Body.String(), expected) {
			t.Errorf("config missing %s: %s", expected, config.Body)
		}
	}
	status := do(t, s, http.MethodGet, "/offline/status", nil)
	if status.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("status Cache-Control = %q", status.Header().Get("Cache-Control"))
	}
	for _, private := range []string{cacheDir, "do-not-expose", "private@example.test", "elevation.invalid"} {
		if strings.Contains(config.Body.String(), private) || strings.Contains(status.Body.String(), private) {
			t.Errorf("private value %q exposed", private)
		}
	}
}
