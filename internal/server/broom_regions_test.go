package server

import (
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
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
	index, err := geojson.UnmarshalFeatureCollection(body)
	require.NoError(t, err)
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
			got := smallestRoutingRegion(index, []orb.Bound{tt.bounds})
			require.NotNil(t, got)
			t.Logf("suggestion: %+v", got)
			assert.Equal(t, tt.want, got.RegionID)
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
	mode := do(t, s, http.MethodPut, "/offline/mode", strings.NewReader(`{"mode":"cache-only"}`))
	require.Equal(t, http.StatusOK, mode.Code)
	result = do(t, s, http.MethodPost, "/offline/routing/suggest", strings.NewReader(request))
	require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	assert.EqualValues(t, 1, calls.Load())
	bad := do(t, s, http.MethodPost, "/offline/routing/suggest", strings.NewReader(`{"bbox":{"south":5,"west":2,"north":3,"east":3}}`))
	assert.Equal(t, http.StatusBadRequest, bad.Code)
}

func TestSmallestRoutingRegion(t *testing.T) {
	index := geojson.NewFeatureCollection()
	add := func(id string, geometry orb.Geometry, downloadable bool) {
		feature := geojson.NewFeature(geometry)
		feature.Properties["id"] = id
		feature.Properties["name"] = id
		if downloadable {
			feature.Properties["urls"] = map[string]any{"pbf": "https://example.test/region.osm.pbf"}
		}
		index.Append(feature)
	}
	square := func(min, max float64) orb.Polygon {
		return orb.Bound{Min: orb.Point{min, min}, Max: orb.Point{max, max}}.ToPolygon()
	}
	add("country", square(0, 10), true)
	add("small", square(1, 4), true)
	add("tiny-no-download", square(2, 3), false)
	view := orb.Bound{Min: orb.Point{2, 2}, Max: orb.Point{3, 3}}
	got := smallestRoutingRegion(index, []orb.Bound{view})
	require.NotNil(t, got)
	assert.Equal(t, "small", got.RegionID)
	got = smallestRoutingRegion(index, []orb.Bound{{Min: orb.Point{2, 2}, Max: orb.Point{5, 5}}})
	require.NotNil(t, got)
	assert.Equal(t, "country", got.RegionID)
	assert.Nil(t, smallestRoutingRegion(index, []orb.Bound{{Min: orb.Point{20, 20}, Max: orb.Point{21, 21}}}))
	withHole := square(1, 4)
	withHole = append(withHole, square(2.2, 2.8)[0])
	assert.False(t, regionCoversView(orb.MultiPolygon{withHole}, view), "hole inside view must not be missed")
	concave := orb.Polygon{{{1, 1}, {4, 1}, {4, 4}, {2.6, 4}, {2.6, 2.5}, {2.4, 2.5}, {2.4, 4}, {1, 4}, {1, 1}}}
	assert.False(t, regionCoversView(orb.MultiPolygon{concave}, view), "concavity between corners must not be missed")
	assert.True(t, regionCoversView(orb.MultiPolygon{square(2, 3)}, view), "identical boundary is covered")
}
