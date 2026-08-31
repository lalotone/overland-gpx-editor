package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRasterMapRejectsHostilePathsBeforeTransport(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errorsNew("unexpected transport")
	})}
	s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	for _, target := range []string{
		"/map/raster/unknown/1/0/0.png", "/map/raster/osm/20/0/0.png",
		"/map/raster/osm/2/4/0.png", "/map/raster/osm/2/0/4.png",
		"/map/raster/osm/2/0/0.jpg", "/map/raster/osm/-1/0/0.png",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Errorf("%s status = %d", target, rec.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("hostile paths made %d transport calls", calls.Load())
	}

	req := httptest.NewRequest(http.MethodGet, "/map/raster/osm/1/0/0.png", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site status = %d", rec.Code)
	}
	if calls.Load() != 0 {
		t.Fatal("cross-site request reached transport")
	}
}

func TestEsriLiveLayersAreNotCacheAdapters(t *testing.T) {
	s := newTestServer(t)
	for _, layer := range []string{"satellite", "relief", "hillshade"} {
		req := httptest.NewRequest(http.MethodGet, "/map/raster/"+layer+"/1/0/0.png", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s cache adapter status = %d, want %d", layer, rec.Code, http.StatusNotFound)
		}
	}
}

func newFakeMapSource(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/styles/liberty":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"version":8,"sprite":%q,"glyphs":%q,"sources":{"vector":{"url":%q},"shade":{"type":"raster","tiles":[%q],"tileSize":256,"maxzoom":0}},"layers":[{"id":"shade","type":"raster","source":"shade"},{"id":"labels","type":"symbol","layout":{"text-font":["Test"]}}]}`, upstream.URL+"/sprites/liberty", upstream.URL+"/fonts/{fontstack}/{range}.pbf", upstream.URL+"/source.json", upstream.URL+"/raster/{z}/{x}/{y}.png")
		case "/source.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tiles":[%q],"type":"vector"}`, upstream.URL+"/tiles/{z}/{x}/{y}.pbf")
		case "/sprites/liberty.json", "/sprites/liberty@2x.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
		case "/sprites/liberty.png", "/sprites/liberty@2x.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\n"))
		default:
			if strings.HasPrefix(r.URL.Path, "/raster/") {
				w.Header().Set("Content-Type", "image/png")
				w.Write([]byte("\x89PNG\r\n\x1a\n"))
			} else if strings.HasPrefix(r.URL.Path, "/tiles/") || strings.HasPrefix(r.URL.Path, "/fonts/Test/") {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Write([]byte("pbf"))
			} else {
				http.NotFound(w, r)
			}
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream, &calls
}

func TestConfiguredOpenFreeMapStyleGraphIsStructuredAndOfflineCapable(t *testing.T) {
	upstream, calls := newFakeMapSource(t)
	cacheDir := t.TempDir()
	newServer := func(mode string) *Server {
		s, err := New(Config{
			GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid",
			OfflineCacheDir: cacheDir, OfflineMode: mode, OpenFreeMapURL: upstream.URL, OpenFreeMapAllowBulk: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	online := newServer("auto")
	requestMap := func(server *Server, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}
	styleRec := requestMap(online, "/map/openfreemap/style.json")
	if styleRec.Code != http.StatusOK {
		t.Fatalf("style = %d %s", styleRec.Code, styleRec.Body)
	}
	if styleRec.Header().Get("X-GPX-Cache") != "miss" {
		t.Fatalf("style cache = %q", styleRec.Header().Get("X-GPX-Cache"))
	}
	var style map[string]any
	if err := json.Unmarshal(styleRec.Body.Bytes(), &style); err != nil {
		t.Fatal(err)
	}
	if style["sprite"] != "/map/openfreemap/sprite" {
		t.Errorf("sprite = %v", style["sprite"])
	}
	if style["glyphs"] != "/map/openfreemap/glyphs/{fontstack}/{range}.pbf" {
		t.Errorf("glyphs = %v", style["glyphs"])
	}
	source := style["sources"].(map[string]any)["vector"].(map[string]any)
	if !strings.HasPrefix(source["url"].(string), "/map/openfreemap/source/") {
		t.Errorf("source URL = %v", source["url"])
	}
	sourceRec := requestMap(online, source["url"].(string))
	if !strings.Contains(sourceRec.Body.String(), "/map/openfreemap/tiles/vector-0/") {
		t.Errorf("source not rewritten: %s", sourceRec.Body)
	}
	if tileRec := requestMap(online, "/map/openfreemap/tiles/vector-0/1/0/0.pbf"); tileRec.Code != 200 || tileRec.Body.String() != "pbf" {
		t.Fatalf("tile = %d %q", tileRec.Code, tileRec.Body)
	}
	packInput := packInput{Name: "map", BBox: &bbox{South: 0, West: 0, North: 1, East: 1}, ZoomMin: 0, ZoomMax: 1, Layers: []string{"openfreemap"}}
	estimate, err := online.packs.estimate(packInput)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.Counts["openfreemap-glyphs"] != 2 {
		t.Fatalf("glyph estimate = %+v", estimate.Counts)
	}
	if estimate.Counts["openfreemap-raster"] != 1 {
		t.Fatalf("raster estimate = %+v", estimate.Counts)
	}
	manifest, _, err := online.packs.start(packInput)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		summary, _ := online.packs.publicManifest(manifest.ID)
		if summary.State == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("map pack did not complete: %+v", summary)
		}
		time.Sleep(time.Millisecond)
	}
	if err := online.Close(); err != nil {
		t.Fatal(err)
	}
	warmedCalls := calls.Load()

	offline := newServer("cache-only")
	defer offline.Close()
	offlineEstimate, err := offline.packs.estimate(packInput)
	if err != nil || offlineEstimate.Counts["openfreemap-raster"] != 1 {
		t.Fatalf("restarted raster estimate = %+v, %v", offlineEstimate.Counts, err)
	}
	if rec := requestMap(offline, "/map/openfreemap/style.json"); rec.Code != 200 || rec.Header().Get("X-GPX-Cache") != "hit" {
		t.Fatalf("offline style = %d %s", rec.Code, rec.Body)
	}
	if rec := requestMap(offline, "/map/openfreemap/tiles/vector-0/1/0/0.pbf"); rec.Code != 200 || rec.Header().Get("X-GPX-Cache") != "hit" {
		t.Fatalf("offline tile = %d cache=%q", rec.Code, rec.Header().Get("X-GPX-Cache"))
	}
	if rec := requestMap(offline, "/map/openfreemap/raster/shade-0/0/0/0.png"); rec.Code != 200 || rec.Header().Get("X-GPX-Cache") != "hit" {
		t.Fatalf("offline raster = %d cache=%q", rec.Code, rec.Header().Get("X-GPX-Cache"))
	}
	if calls.Load() != warmedCalls {
		t.Fatalf("offline startup/request made %d new calls", calls.Load()-warmedCalls)
	}
	if rec := requestMap(offline, "/map/openfreemap/tiles/vector-0/2/3/3.pbf"); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("offline miss = %d", rec.Code)
	}
}

func TestInterruptedMapRefreshPreservesLastCompleteGenerationAcrossRestart(t *testing.T) {
	var broken atomic.Bool
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/styles/liberty":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-cache")
			name, source := "working", upstream.URL+"/source.json"
			if broken.Load() {
				name, source = "broken", upstream.URL+"/missing.json"
			}
			fmt.Fprintf(w, `{"version":8,"name":%q,"sprite":%q,"sources":{"vector":{"url":%q}},"layers":[]}`, name, upstream.URL+"/sprites/liberty", source)
		case "/source.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tiles":[%q],"type":"vector"}`, upstream.URL+"/tiles/{z}/{x}/{y}.pbf")
		case "/sprites/liberty.json", "/sprites/liberty@2x.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
		case "/sprites/liberty.png", "/sprites/liberty@2x.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	cacheDir := t.TempDir()
	newServer := func(mode string) *Server {
		s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: cacheDir, OfflineMode: mode, OpenFreeMapURL: upstream.URL})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	requestStyle := func(s *Server) string {
		req := httptest.NewRequest(http.MethodGet, "/map/openfreemap/style.json", nil)
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("style = %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	first := newServer("auto")
	if style := requestStyle(first); !strings.Contains(style, `"name":"working"`) {
		t.Fatalf("initial generation = %s", style)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	broken.Store(true)
	interrupted := newServer("auto")
	if style := requestStyle(interrupted); !strings.Contains(style, `"name":"working"`) || strings.Contains(style, `"name":"broken"`) {
		t.Fatalf("interrupted generation replaced working graph: %s", style)
	}
	if err := interrupted.Close(); err != nil {
		t.Fatal(err)
	}

	offline := newServer("cache-only")
	defer offline.Close()
	if style := requestStyle(offline); !strings.Contains(style, `"name":"working"`) {
		t.Fatalf("persisted generation = %s", style)
	}
}

func TestPublicOpenFreeMapIsNotAdvertised(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodGet, "/config", nil)
	if strings.Contains(rec.Body.String(), `"openfreemap"`) {
		t.Fatalf("public OpenFreeMap advertised: %s", rec.Body)
	}
}

func TestStyleFontStacks(t *testing.T) {
	style := map[string]any{"layers": []any{
		map[string]any{"layout": map[string]any{"text-font": []any{"Noto Sans Regular", "Noto Sans Italic"}}},
		map[string]any{"layout": map[string]any{"text-font": []any{"literal", []any{"DIN Offc Pro Medium"}}}},
	}}
	got := styleFontStacks(style)
	want := []string{"DIN Offc Pro Medium", "Noto Sans Regular,Noto Sans Italic"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("font stacks = %v, want %v", got, want)
	}
}
