package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoutingCatalogueHierarchyDoesNotPrepareRegions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"id":"spain","name":"Spain","parent":"europe","urls":{"pbf":"https://example.test/spain.pbf"}},"geometry":{"type":"Polygon","coordinates":[[[-5,36],[5,36],[5,44],[-5,44],[-5,36]]]}},{"type":"Feature","properties":{"id":"aragon","name":"Aragón","parent":"spain","urls":{"pbf":"https://example.test/aragon.pbf"}},"geometry":{"type":"Polygon","coordinates":[[[-1,40],[1,40],[1,43],[-1,43],[-1,40]]]}}]}`)
	}))
	defer upstream.Close()
	s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), RoutingCacheDir: t.TempDir(), RoutingIndexURL: upstream.URL})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	response := do(t, s, http.MethodGet, "/offline/routing/regions", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Regions []routingRegionEntry `json:"regions"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(t, result.Regions, 2)
	assert.Equal(t, "region", result.Regions[0].Kind)
	assert.Equal(t, "spain", result.Regions[0].Parent)
	assert.Equal(t, "country", result.Regions[1].Kind)
	assert.NotNil(t, result.Regions[0].BBox)
	assert.False(t, result.Regions[0].Installed)
	assert.Nil(t, s.broom.job)
}
