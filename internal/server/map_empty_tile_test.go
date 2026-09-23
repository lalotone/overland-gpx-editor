package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmptyVectorTilesCompletePackAndSurviveOfflineRestart(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		switch {
		case r.URL.Path == "/styles/liberty":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"version":8,"sources":{"vector":{"type":"vector","tiles":["/tiles/{z}/{x}/{y}.pbf"],"maxzoom":14}},"layers":[]}`)
		case r.URL.Path == "/tiles/14/0/0.pbf":
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/tiles/"):
			// OpenFreeMap uses HTTP 200 with an empty protobuf for tiles with
			// no features. No provider-specific debug header is required.
			w.Header().Set("Content-Type", "application/vnd.mapbox-vector-tile")
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	cacheDir := t.TempDir()
	newServer := func(mode string) *Server {
		s, err := New(Config{
			GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid",
			OfflineCacheDir: cacheDir, OfflineMode: mode, OpenFreeMapURL: upstream.URL, OpenFreeMapAllowBulk: true,
		})
		require.NoError(t, err)
		return s
	}
	online := newServer("auto")
	t.Cleanup(func() { _ = online.Close() })
	input := packInput{Regional: true, Name: "Empty countryside", BBox: &bbox{South: 1, West: 1, North: 1.001, East: 1.001}, ZoomMin: 14, ZoomMax: 14, Layers: []string{"openfreemap"}}
	manifest, estimate, err := online.packs.start(input)
	require.NoError(t, err)
	summary := waitForPackState(t, online.packs, manifest.ID, "complete")
	assert.Zero(t, summary.Failures)
	assert.Equal(t, summary.Total, summary.Done)
	tiles := estimate.MapTiles["vector-0"]
	require.NotEmpty(t, tiles)
	target := fmt.Sprintf("/map/openfreemap/tiles/vector-0/%d/%d/%d.pbf", tiles[0].z, tiles[0].x, tiles[0].y)
	response := do(t, online, http.MethodGet, target, nil)
	require.Equal(t, http.StatusOK, response.Code)
	assert.Empty(t, response.Body.Bytes())
	assert.Equal(t, "hit", response.Header().Get("X-GPX-Cache"))
	missing := do(t, online, http.MethodGet, "/map/openfreemap/tiles/vector-0/14/0/0.pbf", nil)
	assert.Equal(t, http.StatusNotFound, missing.Code, "a missing tile is not an empty tile")
	require.NoError(t, online.Close())
	warmedCalls := calls.Load()

	offline := newServer("cache-only")
	t.Cleanup(func() { _ = offline.Close() })
	response = do(t, offline, http.MethodGet, target, nil)
	require.Equal(t, http.StatusOK, response.Code)
	assert.Empty(t, response.Body.Bytes())
	assert.Equal(t, "hit", response.Header().Get("X-GPX-Cache"))
	restored, ok := offline.packs.publicManifest(manifest.ID)
	require.True(t, ok)
	assert.Equal(t, "complete", restored.State)
	estimate, err = offline.packs.estimate(input)
	require.NoError(t, err)
	assert.Equal(t, estimate.Resources, estimate.Reused, "zero-byte tiles must count as cached resources")
	assert.Equal(t, warmedCalls, calls.Load(), "offline reads must not call the provider")
}

func TestEmptyNonTileMapResourcesRemainInvalid(t *testing.T) {
	for _, tt := range []struct {
		name, params string
		accepted     []string
	}{
		{"glyph", "generation:glyph:Test:0-255", []string{"application/x-protobuf", "application/vnd.mapbox-vector-tile"}},
		{"raster", "generation:resource:shade:14/0/0", []string{"image/png"}},
		{"sprite", "generation:sprite:.png", []string{"image/png"}},
		{"source", "generation:source:vector", []string{"application/json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			validate := openFreeMapValidator(tt.params, tt.accepted)
			require.NotNil(t, validate)
			assert.Error(t, validate(nil))
		})
	}
}
