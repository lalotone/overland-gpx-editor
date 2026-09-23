package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newPackTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	return s
}

func TestTileEnumerationHandlesAntimeridianAndCaps(t *testing.T) {
	tiles, err := enumerateTiles(bbox{South: -1, West: 179, North: 1, East: -179}, 2, 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	seenX := map[int]bool{}
	for _, tile := range tiles {
		seenX[tile.x] = true
	}
	if !seenX[0] || !seenX[3] {
		t.Fatalf("antimeridian x columns = %v", seenX)
	}
	if _, err := enumerateTiles(bbox{South: -80, West: -170, North: 80, East: 170}, 10, 10, 10); err == nil {
		t.Fatal("resource cap was not enforced")
	}
	for _, tile := range tiles {
		bound := 1 << tile.z
		if tile.x < 0 || tile.x >= bound || tile.y < 0 || tile.y >= bound {
			t.Errorf("invalid tile %+v", tile)
		}
	}
}

func TestRoutePackEnumeratesCorridorInsteadOfWholeBounds(t *testing.T) {
	route := []coordinate{{Lat: 0, Lon: 0}, {Lat: 10, Lon: 10}}
	input := packInput{Route: route, PaddingKM: 0}
	bounds := routeBounds(route)
	boxTiles, err := enumerateTiles(bounds, 8, 8, maxPackResources)
	if err != nil {
		t.Fatal(err)
	}
	corridorTiles, err := enumeratePackTiles(input, bounds, 8, 8, maxPackResources)
	if err != nil {
		t.Fatal(err)
	}
	if len(corridorTiles) == 0 || len(corridorTiles) >= len(boxTiles)/2 {
		t.Fatalf("corridor=%d whole-bounds=%d", len(corridorTiles), len(boxTiles))
	}
}

func TestPackPOIBoundsFollowLongAndAntimeridianRoutes(t *testing.T) {
	for name, route := range map[string][]coordinate{
		"long":         {{Lat: 0, Lon: 0}, {Lat: 10, Lon: 10}},
		"antimeridian": {{Lat: 1, Lon: 179}, {Lat: 1, Lon: -179}},
	} {
		t.Run(name, func(t *testing.T) {
			input := packInput{Route: route, PaddingKM: 5}
			sections, err := packPOIBounds(input, paddedBounds(routeBounds(route), input.PaddingKM))
			if err != nil {
				t.Fatal(err)
			}
			if len(sections) < 2 {
				t.Fatalf("POI sections = %d, want a route corridor split", len(sections))
			}
			for _, section := range sections {
				if err := section.validate(250, false); err != nil {
					t.Fatalf("invalid POI section %+v: %v", section, err)
				}
			}
		})
	}
}

func TestUpdatePackResourceSupportsLegacyManifest(t *testing.T) {
	manifest := &packManifest{}
	updatePackResource(manifest, packResourceWater, false, 42, 3)
	if progress := manifest.Resources[packResourceWater]; progress.Done != 1 || progress.Bytes != 42 || progress.Items != 3 {
		t.Fatalf("legacy manifest progress = %+v", progress)
	}
}

func TestPackEstimateDoesNotFetchAndBlocksPublicMaps(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { calls.Add(1); return nil, errorsNew("no fetch") })}
	s, err := New(Config{GPXDir: t.TempDir(), ElevationTiles: true, ElevationTileURL: "https://tiles.invalid/{z}/{x}/{y}.png", ElevationTileCache: t.TempDir(), HTTPClient: client, OfflineCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	input := packInput{Name: "trip", BBox: &bbox{South: 41, West: 179, North: 42, East: -179}, PaddingKM: 2, ZoomMin: 5, ZoomMax: 6, Layers: []string{"osm", "opentopo", "cyclosm"}, Scopes: []string{"elevation"}}
	estimate, err := s.packs.estimate(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(estimate.Blocked) != 3 {
		t.Fatalf("blocked = %+v", estimate.Blocked)
	}
	if estimate.Counts["elevation"] == 0 {
		t.Fatal("elevation tiles were not estimated")
	}
	if calls.Load() != 0 {
		t.Fatalf("estimate made %d transport calls", calls.Load())
	}
}

func TestPackInputRejectsHostileAndOversizedValues(t *testing.T) {
	tests := []packInput{
		{Name: "", BBox: &bbox{South: 1, West: 1, North: 2, East: 2}},
		{Name: "x", BBox: &bbox{South: 2, West: 1, North: 1, East: 2}},
		{Name: "x", BBox: &bbox{South: 1, West: 1, North: 2, East: 2}, Route: []coordinate{{1, 1}, {2, 2}}},
		{Name: strings.Repeat("x", 101), BBox: &bbox{South: 1, West: 1, North: 2, East: 2}},
		{Name: "x", BBox: &bbox{South: 1, West: 1, North: 2, East: 2}, PaddingKM: 101},
		{Name: "x", BBox: &bbox{South: 1, West: 1, North: 2, East: 2}, ZoomMin: 10, ZoomMax: 9},
		{Name: "x", BBox: &bbox{South: 1, West: 1, North: 2, East: 2}, Scopes: []string{"unknown"}},
		{Name: "x", BBox: &bbox{South: 1, West: 1, North: 2, East: 2}, Scopes: []string{"fuel", "fuel"}},
	}
	for i, input := range tests {
		if _, err := validatePackInput(input); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestPackPrefetchesBoundedTripPOIs(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"elements":[{"type":"node","id":1,"lat":40.5,"lon":-0.5,"tags":{}}]}`)
	}))
	defer upstream.Close()
	s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(), OverpassURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	s.providers["pois"].group = newRateGroup(0)
	input := packInput{Name: "pois", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, ZoomMin: 1, ZoomMax: 1, Scopes: []string{"pois"}}
	estimate, err := s.packs.estimate(input)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.Counts["pois-fuel"] != 1 || estimate.Counts["pois-water"] != 1 || estimate.Counts["pois-camp"] != 1 || calls.Load() != 0 {
		t.Fatalf("estimate = %+v, calls = %d", estimate, calls.Load())
	}
	manifest, _, err := s.packs.start(input)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		summary, _ := s.packs.publicManifest(manifest.ID)
		if summary.State == "complete" {
			for _, category := range []string{packResourceFuelStations, packResourceWater, packResourceCampsites} {
				progress := summary.Resources[category]
				if progress.Done != 1 || progress.Total != 1 || progress.Failed != 0 || progress.Items != 1 {
					t.Fatalf("%s progress = %+v", category, progress)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("POI pack did not complete: %+v", summary)
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 3 {
		t.Fatalf("POI calls = %d", calls.Load())
	}
}

func TestFuelPackPersistsPinsAndDeleteReleasesThem(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Fecha":"today","ListaEESSPrecio":[]}`)
	}))
	defer upstream.Close()
	cacheDir := t.TempDir()
	config := Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: cacheDir, OfflineCacheMaxBytes: 32 << 20, FuelURL: upstream.URL}
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := s.packs.start(packInput{Name: "fuel", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, ZoomMin: 1, ZoomMax: 1, Scopes: []string{"fuel"}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		summary, _ := s.packs.publicManifest(manifest.ID)
		if summary.State == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pack did not complete: %+v", summary)
		}
		time.Sleep(time.Millisecond)
	}
	keys := s.cache.keysForScope("fuel")
	if len(keys) != 1 {
		t.Fatalf("fuel keys = %v", keys)
	}
	s.cache.mu.Lock()
	pins := append([]string(nil), s.cache.entries[keys[0]].Pins...)
	s.cache.mu.Unlock()
	if len(pins) != 1 || pins[0] != manifest.ID {
		t.Fatalf("pins = %v", pins)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	config.OfflineMode = "cache-only"
	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	summary, ok := restarted.packs.publicManifest(manifest.ID)
	if !ok || summary.State != "complete" {
		t.Fatalf("restarted pack = %+v, %v", summary, ok)
	}
	if progress := summary.Resources[packResourceFuelPrices]; progress.Done != 1 || progress.Total != 1 || progress.Failed != 0 {
		t.Fatalf("restarted fuel progress = %+v", progress)
	}
	if deleted, err := restarted.packs.delete(manifest.ID); !deleted || err != nil {
		t.Fatal("delete failed")
	}
	restarted.cache.mu.Lock()
	pins = append([]string(nil), restarted.cache.entries[keys[0]].Pins...)
	restarted.cache.mu.Unlock()
	if len(pins) != 0 {
		t.Fatalf("pins after delete = %v", pins)
	}
}

func TestPackStartupReconcilesPinsBeforeLimitEnforcement(t *testing.T) {
	cacheDir := t.TempDir()
	config := Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: cacheDir,
		OfflineCacheMaxBytes: 32 << 20, OfflineCacheMaxEntries: 8,
	}
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	referenced := canonicalCacheKey("p", "s", http.MethodGet, "referenced", "", nil)
	ghostPinned := canonicalCacheKey("p", "s", http.MethodGet, "ghost-pinned", "", nil)
	if err := s.cache.put(testMetadata(referenced, "places", now), []byte("referenced")); err != nil {
		t.Fatal(err)
	}
	if err := s.cache.put(testMetadata(ghostPinned, "places", now.Add(time.Second)), []byte("ghost")); err != nil {
		t.Fatal(err)
	}
	if err := s.cache.pin(ghostPinned, "ffffffffffffffffffffffffffffffff", true); err != nil {
		t.Fatal(err)
	}
	manifest := &packManifest{
		ID: "0123456789abcdef0123456789abcdef", Name: "retained", State: "complete",
		CreatedAt: now, UpdatedAt: now, Done: 1, Total: 1,
		Resources: map[string]packResourceProgress{packResourcePlaces: {Done: 1, Total: 1}},
		CacheKeys: []string{referenced},
	}
	s.packs.mu.Lock()
	err = s.packs.persistLocked(manifest)
	s.packs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	config.OfflineCacheMaxEntries = 1
	config.OfflineMode = "cache-only"
	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.cache.mu.Lock()
	referencedMeta := restarted.cache.entries[referenced]
	_, ghostRemains := restarted.cache.entries[ghostPinned]
	var pins []string
	if referencedMeta != nil {
		pins = append(pins, referencedMeta.Pins...)
	}
	restarted.cache.mu.Unlock()
	if referencedMeta == nil || ghostRemains || fmt.Sprint(pins) != "[0123456789abcdef0123456789abcdef]" {
		t.Fatalf("reconciled startup: referenced=%v ghost=%v pins=%v", referencedMeta != nil, ghostRemains, pins)
	}
	if summary, ok := restarted.packs.publicManifest(manifest.ID); !ok || summary.State != "complete" {
		t.Fatalf("restored manifest = %+v, %v", summary, ok)
	}
}

func TestPackWarmRestartDoesNotRewriteReconciledSidecar(t *testing.T) {
	cacheDir := t.TempDir()
	config := Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: cacheDir,
		OfflineCacheMaxBytes: 32 << 20,
	}
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	key := canonicalCacheKey("p", "s", http.MethodGet, "warm", "", nil)
	if err := s.cache.put(testMetadata(key, "places", now), []byte("warm")); err != nil {
		t.Fatal(err)
	}
	manifest := &packManifest{
		ID: "0123456789abcdef0123456789abcdef", Name: "warm", State: "complete",
		CreatedAt: now, UpdatedAt: now, Done: 1, Total: 1,
		Resources: map[string]packResourceProgress{packResourcePlaces: {Done: 1, Total: 1}},
		CacheKeys: []string{key},
	}
	if err := s.cache.pin(key, manifest.ID, true); err != nil {
		t.Fatal(err)
	}
	s.packs.mu.Lock()
	err = s.packs.persistLocked(manifest)
	s.packs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, metaPath, err := s.cache.paths("places", key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(946684800, 0)
	if err := os.Chtimes(metaPath, fixed, fixed); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	info, err := os.Stat(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(fixed) {
		t.Fatalf("warm restart rewrote unchanged sidecar: modtime=%s", info.ModTime())
	}
}

func TestPackStartupRejectsMismatchedManifestIdentity(t *testing.T) {
	cacheDir := t.TempDir()
	config := Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: cacheDir}
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	filenameID := "0123456789abcdef0123456789abcdef"
	embeddedID := "fedcba9876543210fedcba9876543210"
	manifest := packManifest{
		ID: embeddedID, Name: "mismatch", State: "complete", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Resources: map[string]packResourceProgress{packResourcePlaces: {Done: 1, Total: 1}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFileAt(s.cache.root, s.cache.tmpRel, filepath.Join(s.cache.packsRel, filenameID+".json"), raw); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if summaries := restarted.packs.summaries(); len(summaries) != 0 {
		t.Fatalf("mismatched manifest was loaded: %+v", summaries)
	}
	if _, err := restarted.cache.root.Lstat(filepath.Join(restarted.cache.packsRel, embeddedID+".json")); !os.IsNotExist(err) {
		t.Fatalf("mismatched manifest created alternate identity: %v", err)
	}
}

func TestLegacyCompleteManifestIsRepreparedWithoutChangingRetention(t *testing.T) {
	cacheDir := t.TempDir()
	config := Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: cacheDir}
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	legacy := &packManifest{
		ID: id, Name: "old pack", State: "complete", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Done: 1, Total: 1, Input: packInput{Name: "old pack", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"fuel"}},
	}
	automaticID := "fedcba9876543210fedcba9876543210"
	automatic := &packManifest{
		ID: automaticID, Name: "Route: Existing", State: "complete", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Done: 1, Total: 1, Resources: map[string]packResourceProgress{packResourceFuelPrices: {Done: 1, Total: 1}},
		Input: packInput{Name: "Route: Existing", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"fuel"}},
	}
	s.packs.mu.Lock()
	err = s.packs.persistLocked(legacy)
	if err == nil {
		err = s.packs.persistLocked(automatic)
	}
	s.packs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	summary, ok := restarted.packs.publicManifest(id)
	if !ok || summary.State != "incomplete" || summary.ErrorCode != "legacy_manifest" {
		t.Fatalf("legacy pack = %+v, found=%v", summary, ok)
	}
	restarted.packs.mu.Lock()
	legacyRoute := restarted.packs.packs[automaticID]
	restarted.packs.mu.Unlock()
	if legacyRoute == nil || legacyRoute.Input.Automatic {
		t.Fatalf("legacy route retention changed: %+v", legacyRoute)
	}
	reused, _, err := restarted.packs.start(packInput{
		Name: "Route: Existing", Automatic: true, BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"fuel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reused.ID != automaticID || legacyRoute.Input.Automatic {
		t.Fatalf("legacy route reuse = %s, automatic=%v", reused.ID, legacyRoute.Input.Automatic)
	}
}

func TestPackHTTPContractAcceptsFrontendAliases(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Fecha":"today","ListaEESSPrecio":[]}`)
	}))
	defer upstream.Close()
	s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(), FuelURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	body := `{"name":"frontend","bbox":[40,-1,41,0],"paddingKm":5,"minZoom":8,"maxZoom":9,"layers":["osm"],"scopes":["fuel"]}`
	req := httptest.NewRequest(http.MethodPost, "/offline/packs/estimate", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("estimate = %d %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest(http.MethodPost, "/offline/packs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var summary map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["id"] == nil || summary["state"] == nil {
		t.Fatalf("creation response is not a pack summary: %v", summary)
	}
	if summary["pack"] != nil {
		t.Fatalf("unexpected response wrapper: %v", summary)
	}
	bounds, ok := summary["bbox"].(map[string]any)
	if !ok || bounds["south"] != float64(40) || bounds["west"] != float64(-1) ||
		bounds["north"] != float64(41) || bounds["east"] != float64(0) {
		t.Fatalf("creation response lost area bounds: %v", summary)
	}
	if strings.Contains(rec.Body.String(), `"input"`) || strings.Contains(rec.Body.String(), `"cacheKeys"`) ||
		strings.Contains(rec.Body.String(), `"route"`) {
		t.Fatalf("creation response exposed private manifest data: %s", rec.Body)
	}
}

func TestPackStartReusesIdenticalActiveRoute(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Fecha":"today","ListaEESSPrecio":[]}`)
	}))
	defer upstream.Close()
	s, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(), FuelURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	input := packInput{Name: "automatic route", Route: []coordinate{{Lat: 40, Lon: -1}, {Lat: 40.1, Lon: -0.9}}, PaddingKM: 5, ZoomMin: 8, ZoomMax: 14, Scopes: []string{"fuel"}}
	first, _, err := s.packs.start(input)
	if err != nil {
		t.Fatal(err)
	}
	waitForPackState(t, s.packs, first.ID, "complete")
	second, _, err := s.packs.start(input)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate pack id = %s, want %s", second.ID, first.ID)
	}
	if calls.Load() != 1 {
		t.Fatalf("identical pack fetched %d times", calls.Load())
	}
}

func TestPackStartEnforcesActiveCeilingBeforeEstimating(t *testing.T) {
	s := newPackTestServer(t)
	input := packInput{Name: "capacity", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"fuel"}}
	s.packs.mu.Lock()
	for i := 0; i < maxActivePackJobs; i++ {
		id := fmt.Sprintf("%032x", i+1)
		s.packs.packs[id] = &packManifest{ID: id, State: "running"}
	}
	s.packs.mu.Unlock()
	if _, _, err := s.packs.start(input); err == nil || !strings.Contains(err.Error(), "pack jobs") {
		t.Fatalf("active capacity error = %v", err)
	}

}

func TestAutomaticPacksDoNotEvictAtFormerCountLimit(t *testing.T) {
	s := newPackTestServer(t)
	oldestID := fmt.Sprintf("%032x", 1)
	s.packs.mu.Lock()
	for i := 0; i < 32; i++ {
		id := fmt.Sprintf("%032x", i+1)
		s.packs.packs[id] = &packManifest{
			ID: id, State: "complete", UpdatedAt: time.Unix(int64(i+1), 0),
			Input: packInput{Automatic: true},
		}
	}
	s.packs.mu.Unlock()
	created, _, err := s.packs.start(packInput{
		Name: "new route", Automatic: true, BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"fuel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.packs.mu.Lock()
	_, keptOldest := s.packs.packs[oldestID]
	_, keptCreated := s.packs.packs[created.ID]
	stored := len(s.packs.packs)
	s.packs.mu.Unlock()
	if !keptOldest || !keptCreated || stored != 33 {
		t.Fatalf("automatic retention: oldest=%v created=%v stored=%d", keptOldest, keptCreated, stored)
	}
}

func TestPackManagerRetainsMoreThanSixteenManifestsAfterRestart(t *testing.T) {
	cacheDir := t.TempDir()
	config := Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: cacheDir}
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	oldestID := fmt.Sprintf("%032x", 1)
	s.packs.mu.Lock()
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("%032x", i+1)
		manifest := &packManifest{
			ID: id, Name: fmt.Sprintf("automatic %d", i), State: "complete", CreatedAt: time.Unix(int64(i+1), 0), UpdatedAt: time.Unix(int64(i+1), 0),
			Resources: map[string]packResourceProgress{packResourceFuelPrices: {Done: 1, Total: 1}}, Input: packInput{Automatic: true},
		}
		if err := s.packs.persistLocked(manifest); err != nil {
			s.packs.mu.Unlock()
			t.Fatal(err)
		}
	}
	s.packs.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, ok := restarted.packs.publicManifest(oldestID); !ok || len(restarted.packs.summaries()) != 40 {
		t.Fatalf("repaired manifests: oldest=%v stored=%d", ok, len(restarted.packs.summaries()))
	}
	entries, err := os.ReadDir(restarted.cache.packsDir)
	if err != nil || len(entries) != 40 {
		t.Fatalf("repaired manifest files = %d, %v", len(entries), err)
	}
}

func TestPackReservesManifestQuota(t *testing.T) {
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
		OfflineCacheMaxBytes: int64(maxRegionalManifestBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	input := packInput{Name: "fuel", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"fuel"}}
	if _, _, err := s.packs.start(input); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("reserved quota error = %v", err)
	}
}

func TestElevationPackHasSeparateTileCeiling(t *testing.T) {
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationTiles: true, ElevationTileURL: "https://tiles.invalid/{z}/{x}/{y}.png",
		ElevationTileCache: t.TempDir(), OfflineCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	input := packInput{
		Name: "large elevation", BBox: &bbox{South: 40, West: -2, North: 42, East: 0},
		Scopes: []string{"elevation"},
	}
	if _, err := s.packs.estimate(input); err == nil || !strings.Contains(err.Error(), "elevation pack exceeds") {
		t.Fatalf("elevation ceiling error = %v", err)
	}
}

func TestElevationPackHonorsConfiguredTileCacheQuota(t *testing.T) {
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationTiles: true, ElevationTileURL: "https://tiles.invalid/{z}/{x}/{y}.png",
		ElevationTileCache: t.TempDir(), ElevationTileCacheMaxBytes: 1, OfflineCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	input := packInput{
		Name: "quota", BBox: &bbox{South: 40, West: -1, North: 40.01, East: -0.99},
		Scopes: []string{"elevation"},
	}
	if _, err := s.packs.estimate(input); err == nil || !strings.Contains(err.Error(), "tile cache quota") {
		t.Fatalf("tile quota error = %v", err)
	}
}

func TestElevationPackCompletesWithinByteLimit(t *testing.T) {
	tiles := newTileServer(t)
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationTiles: true, ElevationTileURL: tiles.url(),
		ElevationTileCache: t.TempDir(), OfflineCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	manifest, _, err := s.packs.start(packInput{
		Name:   "small elevation",
		BBox:   &bbox{South: 41.999, West: -0.501, North: 42.001, East: -0.499},
		Scopes: []string{"elevation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.packs.mu.Lock()
		state := s.packs.packs[manifest.ID].State
		s.packs.mu.Unlock()
		if state == "complete" {
			break
		}
		if state != "queued" && state != "running" {
			t.Fatalf("elevation pack state = %q", state)
		}
		if time.Now().After(deadline) {
			t.Fatal("elevation pack did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPackPersistenceFailureBecomesStorageError(t *testing.T) {
	s := newPackTestServer(t)
	id := strings.Repeat("a", 32)
	manifest := &packManifest{ID: id, Name: "trip", State: "running", CreatedAt: time.Now().UTC()}
	s.packs.mu.Lock()
	s.packs.packs[id] = manifest
	if err := s.packs.persistLocked(manifest); err != nil {
		s.packs.mu.Unlock()
		t.Fatal(err)
	}
	s.packs.mu.Unlock()

	oldPacks := s.cache.packsDir + ".old"
	if err := os.Rename(s.cache.packsDir, oldPacks); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), s.cache.packsDir); err != nil {
		t.Fatal(err)
	}
	if applied := s.packs.update(manifest, func(pack *packManifest) { pack.Done++ }); applied {
		t.Fatal("pack update reported durable success")
	}
	if manifest.State != "incomplete" || manifest.ErrorCode != "storage_error" {
		t.Fatalf("manifest after storage failure = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(oldPacks, id+".json")); err != nil {
		t.Fatalf("original manifest was not preserved: %v", err)
	}
}

func TestPackHardStopsWhenActualResourceExceedsBudget(t *testing.T) {
	var calls atomic.Int64
	oversized := `{"elements":[],"padding":"` + strings.Repeat("x", 6<<20) + `"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, oversized)
	}))
	defer upstream.Close()
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
		OfflineCacheMaxBytes: int64(maxRegionalManifestBytes + maxPackManifestBytes + 5<<20), OverpassURL: upstream.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	s.providers["pois"].group = newRateGroup(0)
	manifest, _, err := s.packs.start(packInput{
		Name: "oversized", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"pois"},
	})
	if err != nil {
		t.Fatal(err)
	}
	summary := waitForPackState(t, s.packs, manifest.ID, "incomplete")
	if summary.ErrorCode != "resource_limit" || summary.Failures != 1 || summary.Bytes != 0 {
		t.Fatalf("oversized pack = %+v", summary)
	}
	if progress := summary.Resources[packResourceFuelStations]; progress.Done != 1 || progress.Total != 1 || progress.Failed != 1 {
		t.Fatalf("failed fuel-station progress = %+v", progress)
	}
	if calls.Load() != 1 {
		t.Fatalf("pack continued after oversized resource: calls=%d", calls.Load())
	}
	if s.cache.stats().Entries != 0 {
		t.Fatal("oversized resource was admitted to the cache")
	}
}

func TestPackContinuesWithOtherResourcesAfterProviderFailure(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"elements":`)
			return
		}
		fmt.Fprint(w, `{"elements":[]}`)
	}))
	defer upstream.Close()
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(), OverpassURL: upstream.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	s.providers["pois"].group = newRateGroup(0)
	manifest, _, err := s.packs.start(packInput{
		Name: "partial POIs", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"pois"},
	})
	if err != nil {
		t.Fatal(err)
	}
	summary := waitForPackState(t, s.packs, manifest.ID, "incomplete")
	if summary.ErrorCode != "resource_failures" || summary.Done != 3 || summary.Total != 3 || summary.Failures != 1 {
		t.Fatalf("partial pack = %+v", summary)
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want all three POI resources", calls.Load())
	}
	if progress := summary.Resources[packResourceFuelStations]; progress.Failed != 1 {
		t.Fatalf("fuel progress = %+v", progress)
	}
	for _, category := range []string{packResourceWater, packResourceCampsites} {
		if progress := summary.Resources[category]; progress.Done != 1 || progress.Failed != 0 {
			t.Fatalf("%s progress = %+v", category, progress)
		}
	}
}

func TestPackWholeJobDeadlineCancelsUpstreamWork(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	requestDone := make(chan struct{}, 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestStarted <- struct{}{}
		<-r.Context().Done()
		requestDone <- struct{}{}
		return nil, r.Context().Err()
	})}
	s, err := New(Config{
		GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
		OverpassURL: "http://overpass.invalid", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	s.providers["pois"].group = newRateGroup(0)
	s.packs.jobTimeout = 200 * time.Millisecond
	manifest, _, err := s.packs.start(packInput{
		Name: "deadline", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"pois"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("pack did not start its upstream request")
	}
	summary := waitForPackState(t, s.packs, manifest.ID, "incomplete")
	if summary.ErrorCode != "deadline" {
		t.Fatalf("deadline pack = %+v", summary)
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("job deadline did not cancel the upstream request")
	}
}

func waitForPackState(t *testing.T, packs *packManager, id, state string) packSummary {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		summary, ok := packs.publicManifest(id)
		if !ok {
			t.Fatal("pack disappeared")
		}
		if summary.State == state {
			return summary
		}
		if summary.State != "queued" && summary.State != "running" {
			t.Fatalf("pack reached %q, want %q: %+v", summary.State, state, summary)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pack did not reach %q: %+v", state, summary)
		}
		time.Sleep(time.Millisecond)
	}
}
