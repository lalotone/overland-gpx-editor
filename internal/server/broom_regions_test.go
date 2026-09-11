package server

import (
	"github.com/paulmach/orb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// Run against the unmodified Geofabrik catalogue, not mock rectangles:
// ROUTING_TEST_INDEX=/path/to/index-v1.json go test ./internal/server -run TestRealRoutingCatalogue -v
func TestRealRoutingCatalogue(t *testing.T) {
	filename := os.Getenv("ROUTING_TEST_INDEX")
	if filename == "" {
		t.Skip("set ROUTING_TEST_INDEX to a downloaded Geofabrik catalogue")
	}
	body, err := os.ReadFile(filename)
	require.NoError(t, err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(upstream.Close)
	s, err := New(Config{GPXDir: t.TempDir(), RoutingCacheDir: t.TempDir(), OfflineCacheDir: t.TempDir(), RoutingIndexURL: upstream.URL + "/index.json"})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	for _, tt := range []struct {
		name   string
		bounds orb.Bound
		want   string
	}{
		{"Catalonia screenshot", orb.Bound{Min: orb.Point{0.55, 40.9}, Max: orb.Point{3.95, 42.4}}, "cataluna"},
		{"Barcelona", orb.Bound{Min: orb.Point{2.05, 41.3}, Max: orb.Point{2.25, 41.5}}, "cataluna"},
		{"Zaragoza", orb.Bound{Min: orb.Point{-1, 41.5}, Max: orb.Point{-0.7, 41.8}}, "aragon"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.broom.manager.SuggestRegions(t.Context(), tt.bounds)
			require.NoError(t, err)
			require.NotEmpty(t, got)
			t.Logf("suggestion: %s, covers view: %v", got[0].Region.Name, got[0].CoversEntireArea)
			assert.Equal(t, tt.want, got[0].Region.ID)
		})
	}
}

func TestRoutingSuggestionCachesOnlyCatalogue(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/index.json", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"id":"test-region","name":"Test Region","urls":{"pbf":"https://example.test/test.osm.pbf"}},"geometry":{"type":"Polygon","coordinates":[[[0,0],[10,0],[10,10],[0,10],[0,0]]]}}]}`))
	}))
	t.Cleanup(upstream.Close)
	s, err := New(Config{GPXDir: t.TempDir(), RoutingCacheDir: t.TempDir(), OfflineCacheDir: t.TempDir(), RoutingIndexURL: upstream.URL + "/index.json"})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	request := `{"bbox":{"south":2,"west":2,"north":3,"east":3}}`
	result := do(t, s, http.MethodPost, "/offline/routing/suggest", strings.NewReader(request))
	require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	assert.Contains(t, result.Body.String(), `"regionId":"test-region"`)
	assert.Nil(t, s.broom.job, "suggesting a download must not start preparation")
	plan := do(t, s, http.MethodPost, "/offline/routing/plan", strings.NewReader(`{"regionId":"test-region"}`))
	require.Equal(t, http.StatusOK, plan.Code, plan.Body.String())
	assert.Contains(t, plan.Body.String(), `"tilesKnown":false`)
	assert.Contains(t, plan.Body.String(), `"estimatedBytes":null`)
	mode := do(t, s, http.MethodPut, "/offline/mode", strings.NewReader(`{"mode":"cache-only"}`))
	require.Equal(t, http.StatusOK, mode.Code)
	result = do(t, s, http.MethodPost, "/offline/routing/suggest", strings.NewReader(request))
	require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	assert.EqualValues(t, 1, calls.Load())
	bad := do(t, s, http.MethodPost, "/offline/routing/suggest", strings.NewReader(`{"bbox":{"south":5,"west":2,"north":3,"east":3}}`))
	assert.Equal(t, http.StatusBadRequest, bad.Code)
}
