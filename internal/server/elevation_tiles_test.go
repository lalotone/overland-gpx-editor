package server

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// encodeTerrarium builds a tile whose every pixel carries `ele` metres, in the
// same encoding the real service uses.
func encodeTerrarium(t *testing.T, ele func(x, y int) float64) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, tileSize, tileSize))
	for y := 0; y < tileSize; y++ {
		for x := 0; x < tileSize; x++ {
			v := ele(x, y) + 32768
			r := int(v / 256)
			g := int(v) % 256
			b := int((v - float64(int(v))) * 256)
			img.Set(x, y, color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tileServer serves synthetic tiles and counts requests per tile.
type tileServer struct {
	*httptest.Server
	requests atomic.Int64
	mu       sync.Mutex
	seen     map[string]int
	missing  map[string]bool
	ele      func(x, y int) float64
}

func newTileServer(t *testing.T) *tileServer {
	t.Helper()
	ts := &tileServer{
		seen:    map[string]int{},
		missing: map[string]bool{},
		ele:     func(x, y int) float64 { return 500 },
	}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		ts.requests.Add(1)
		ts.mu.Lock()
		ts.seen[path]++
		missing := ts.missing[path]
		ele := ts.ele
		ts.mu.Unlock()

		if missing {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(encodeTerrarium(t, ele))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *tileServer) url() string { return ts.Server.URL + "/{z}/{x}/{y}.png" }

func newTileServerStore(t *testing.T, ts *tileServer, cacheDir string) *tileStore {
	t.Helper()
	return newTileStore(ts.url(), defaultTileZoom, cacheDir, ts.Client())
}

// The whole point of the encoding: metres survive the round trip, including
// the sub-metre part carried in the blue channel.
func TestTerrariumDecode(t *testing.T) {
	for _, want := range []float64{0, 1, 500, 1939.5, 8848, -412} {
		raw := encodeTerrarium(t, func(x, y int) float64 { return want })
		grid, err := decodeTerrarium(raw)
		if err != nil {
			t.Fatalf("%v: %v", want, err)
		}
		if got := grid.at(10, 10); got < want-0.01 || got > want+0.01 {
			t.Errorf("decoded %.3f, want %.3f", got, want)
		}
	}
}

func TestTileSampleIsBilinear(t *testing.T) {
	ts := newTileServer(t)
	// A west-to-east ramp: one metre per pixel column.
	ts.ele = func(x, y int) float64 { return float64(x) }
	s := newTileServerStore(t, ts, "")

	// Two points a fraction of a pixel apart must return values between the
	// posts, not the same terraced reading.
	n := float64(int(1) << defaultTileZoom)
	lonPerPixel := 360 / (n * tileSize)

	base := point{lat: 42.0, lon: -0.5}
	shifted := point{lat: 42.0, lon: -0.5 + lonPerPixel/2}

	a, err := s.sample(context.Background(), base)
	if err != nil || a == nil {
		t.Fatalf("sample: %v %v", a, err)
	}
	b, err := s.sample(context.Background(), shifted)
	if err != nil || b == nil {
		t.Fatalf("sample: %v %v", b, err)
	}
	diff := *b - *a
	if diff < 0.4 || diff > 0.6 {
		t.Errorf("half a pixel east moved elevation by %.3f m, want ~0.5 — not interpolating", diff)
	}
}

// A batch lands in a handful of tiles; each must be fetched once even though
// hundreds of points ask for it, and again zero times on a second pass.
func TestTilesAreFetchedOnceAndCached(t *testing.T) {
	ts := newTileServer(t)
	s := newTileServerStore(t, ts, "")

	points := make([]point, 300)
	for i := range points {
		points[i] = point{lat: 42.0 + float64(i)*1e-5, lon: -0.5 + float64(i)*1e-5}
	}

	if _, err := s.lookup(context.Background(), points); err != nil {
		t.Fatal(err)
	}
	first := ts.requests.Load()
	if first == 0 {
		t.Fatal("no tiles fetched")
	}
	if first > 8 {
		t.Errorf("%d tile requests for a 300-point cluster — not deduplicating", first)
	}
	for path, n := range ts.seen {
		if n != 1 {
			t.Errorf("tile %s fetched %d times", path, n)
		}
	}

	if _, err := s.lookup(context.Background(), points); err != nil {
		t.Fatal(err)
	}
	if ts.requests.Load() != first {
		t.Errorf("second pass refetched: %d then %d", first, ts.requests.Load())
	}
}

func TestConcurrentSamplesShareOneFetch(t *testing.T) {
	ts := newTileServer(t)
	s := newTileServerStore(t, ts, "")

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.sample(context.Background(), point{lat: 42.0, lon: -0.5})
		}()
	}
	wg.Wait()

	// One tile, or up to four if the point sits on a tile seam.
	if n := ts.requests.Load(); n > 4 {
		t.Errorf("%d concurrent fetches of the same tile", n)
	}
}

// Missing coverage must read as unknown, never as sea level.
func TestMissingTileYieldsNilNotZero(t *testing.T) {
	ts := newTileServer(t)
	s := newTileServerStore(t, ts, "")

	p := point{lat: 42.0, lon: -0.5}
	px, py := s.project(p)
	key := tileKey{z: defaultTileZoom, x: int(px) / tileSize, y: int(py) / tileSize}
	ts.mu.Lock()
	ts.missing[key.path()] = true
	ts.mu.Unlock()

	got, err := s.sample(context.Background(), p)
	if err != nil {
		t.Fatalf("a missing tile is not an error: %v", err)
	}
	if got != nil {
		t.Errorf("got %.1f for an uncovered point, want nil", *got)
	}
}

func TestUpstreamErrorIsReported(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(stub.Close)

	s := newTileStore(stub.URL+"/{z}/{x}/{y}.png", defaultTileZoom, "", stub.Client())
	if _, err := s.sample(context.Background(), point{lat: 42, lon: -0.5}); err == nil {
		t.Fatal("want an error when the tile server fails")
	}
}

// The disk cache is what makes elevation work with no signal.
func TestDiskCacheSurvivesRestart(t *testing.T) {
	ts := newTileServer(t)
	dir := t.TempDir()

	s := newTileServerStore(t, ts, dir)
	first, err := s.sample(context.Background(), point{lat: 42, lon: -0.5})
	if err != nil || first == nil {
		t.Fatalf("sample: %v %v", first, err)
	}
	fetched := ts.requests.Load()

	var onDisk int
	filepath.Walk(dir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".png") {
			onDisk++
		}
		return nil
	})
	if onDisk == 0 {
		t.Fatal("nothing written to the tile cache")
	}

	// A fresh store with the same directory, and the network taken away.
	offline := newTileStore(ts.url(), defaultTileZoom, dir, &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("offline")
		}),
	})
	second, err := offline.sample(context.Background(), point{lat: 42, lon: -0.5})
	if err != nil {
		t.Fatalf("cached tiles must serve with no network: %v", err)
	}
	if second == nil || *second != *first {
		t.Errorf("offline read %v, want %v", second, *first)
	}
	if ts.requests.Load() != fetched {
		t.Error("offline store hit the network")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The provider seam: with tiles configured the HTTP API is unchanged, and the
// dataset it reports names the real source.
func TestServerUsesTilesWhenEnabled(t *testing.T) {
	ts := newTileServer(t)
	ts.ele = func(x, y int) float64 { return 1234 }

	s, err := New(Config{
		GPXDir:           t.TempDir(),
		ElevationTiles:   true,
		ElevationTileURL: ts.url(),
		ElevationHost:    "http://should-not-be-used.invalid",
		ElevationDataset: "srtm30m",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := postBatch(t, s, `{"locations":"42.0,-0.5|42.001,-0.501"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	got := decodeBatch(t, rec)
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(got.Results))
	}
	for i, r := range got.Results {
		if r.Elevation == nil || *r.Elevation < 1233.9 || *r.Elevation > 1234.1 {
			t.Errorf("result %d = %v, want ~1234", i, r.Elevation)
		}
	}
	if got.Dataset != tileDataset {
		t.Errorf("dataset = %q, want %q", got.Dataset, tileDataset)
	}
}

/* -- Prefetch --------------------------------------------------------- */

func waitForPrefetch(t *testing.T, s *tileStore) prefetchProgress {
	t.Helper()
	for i := 0; i < 200; i++ {
		p := s.progress()
		if !p.Running {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("prefetch did not finish")
	return prefetchProgress{}
}

func TestPrefetchWarmsTheAreaAhead(t *testing.T) {
	ts := newTileServer(t)
	s := newTileServerStore(t, ts, "")

	// A small box around Zaragoza.
	started := s.startPrefetch(41.60, -0.95, 41.70, -0.82)
	if !started.Running || started.Total == 0 {
		t.Fatalf("prefetch did not start: %+v", started)
	}
	done := waitForPrefetch(t, s)
	if done.Done != started.Total {
		t.Errorf("finished at %d/%d", done.Done, started.Total)
	}
	if int(ts.requests.Load()) != started.Total {
		t.Errorf("fetched %d tiles for a %d-tile area", ts.requests.Load(), started.Total)
	}

	// The point of it: a lookup inside that box now costs no network at all.
	before := ts.requests.Load()
	v, err := s.sample(context.Background(), point{lat: 41.65, lon: -0.88})
	if err != nil || v == nil {
		t.Fatalf("sample after prefetch: %v %v", v, err)
	}
	if ts.requests.Load() != before {
		t.Error("a prefetched area still hit the network on lookup")
	}
}

// Zoomed out far enough, the viewport is thousands of tiles; that must be
// refused rather than quietly pulling hundreds of megabytes.
func TestPrefetchRefusesTooWideAnArea(t *testing.T) {
	ts := newTileServer(t)
	s := newTileServerStore(t, ts, "")

	// Roughly Zaragoza to Teruel and well beyond, at z13.
	got := s.startPrefetch(39.5, -2.5, 42.5, 0.5)
	if !got.Skipped {
		t.Fatalf("expected the area to be refused, got %+v", got)
	}
	if got.Reason == "" {
		t.Error("a refusal with no reason cannot be shown to anyone")
	}
	if ts.requests.Load() != 0 {
		t.Errorf("%d tiles fetched despite refusing", ts.requests.Load())
	}
}

// Panning replaces the previous target rather than queueing both.
func TestPrefetchReplacesThePreviousArea(t *testing.T) {
	ts := newTileServer(t)
	s := newTileServerStore(t, ts, "")

	s.startPrefetch(41.60, -0.95, 41.70, -0.82)
	second := s.startPrefetch(40.30, -1.20, 40.40, -1.05)
	if !second.Running {
		t.Fatalf("second prefetch did not start: %+v", second)
	}
	done := waitForPrefetch(t, s)
	if done.Total != second.Total {
		t.Errorf("progress reports %d total, want the newest request's %d", done.Total, second.Total)
	}
}

func TestPrefetchEndpointsReportDisabledWithoutTiles(t *testing.T) {
	s := newTestServer(t) // no ElevationTiles
	rec := do(t, s, http.MethodPost, "/elevation/prefetch", strings.NewReader(`{"bbox":[41.6,-0.9,41.7,-0.8]}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	rec = do(t, s, http.MethodGet, "/elevation/prefetch", nil)
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("GET = %s", rec.Body)
	}
}

func TestPrefetchEndpointValidatesBbox(t *testing.T) {
	ts := newTileServer(t)
	s, err := New(Config{GPXDir: t.TempDir(), ElevationTiles: true, ElevationTileURL: ts.url()})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"bbox":[1,2]}`, `{"bbox":[95,0,96,1]}`, `nonsense`} {
		rec := do(t, s, http.MethodPost, "/elevation/prefetch", strings.NewReader(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
}
