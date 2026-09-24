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

func TestVectorPackUsesNativeZoomWhenOverzoomed(t *testing.T) {
	var rejected atomic.Int32
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/style.json" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"version":8,"sources":{"v":{"type":"vector","maxzoom":2,"tiles":[%q]}},"layers":[]}`, upstream.URL+"/tiles/{z}/{x}/{y}.pbf")
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/tiles/2/") {
			rejected.Add(1)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write([]byte("pbf"))
	}))
	defer upstream.Close()
	s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), OpenFreeMapURL: upstream.URL + "/style.json", OpenFreeMapAllowBulk: true})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	input := packInput{Name: "overzoom", BBox: &bbox{South: 1, West: 1, North: 2, East: 2}, ZoomMin: 3, ZoomMax: 4, Layers: []string{"openfreemap"}}
	estimate, err := s.packs.estimateContext(t.Context(), input)
	require.NoError(t, err)
	require.NotEmpty(t, estimate.MapTiles)
	for _, tiles := range estimate.MapTiles {
		for _, tile := range tiles {
			assert.Equal(t, 2, tile.z)
		}
	}
	pack, _, err := s.packs.startContext(t.Context(), input)
	require.NoError(t, err)
	summary := waitForPackState(t, s.packs, pack.ID, "complete")
	assert.Zero(t, summary.Failures)
	assert.Zero(t, rejected.Load(), "overzoom must not request nonexistent upstream tiles")
}
