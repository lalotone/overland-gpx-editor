package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackDistinguishesCachedDownloadedAndRevalidatedMaps(t *testing.T) {
	var requests atomic.Int32
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		if r.URL.Path == "/style.json" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"version":8,"sources":{"v":{"type":"vector","maxzoom":0,"tiles":[%q]}},"layers":[]}`, upstream.URL+"/vector/{z}/{x}/{y}.pbf")
			return
		}
		requests.Add(1)
		w.Header().Set("ETag", `"one"`)
		if r.Header.Get("If-None-Match") == `"one"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write([]byte("pbf"))
	}))
	t.Cleanup(upstream.Close)
	s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), OpenFreeMapURL: upstream.URL + "/style.json", OpenFreeMapAllowBulk: true})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	var input packInput
	require.NoError(t, json.Unmarshal([]byte(`{"name":"First","coverageKind":"region","bbox":[40,-1,41,0],"zoomMin":0,"zoomMax":0,"layers":["openfreemap"]}`), &input))
	run := func(name string) packSummary {
		input.Name = name
		pack, _, err := s.packs.start(input)
		require.NoError(t, err)
		return waitForPackState(t, s.packs, pack.ID, "complete")
	}
	first := run("First")
	assert.Equal(t, "region", first.CoverageKind)
	assert.EqualValues(t, 1, requests.Load())
	assert.Equal(t, 1, first.Resources[packResourceVectorMap].Downloaded)
	second := run("Retry missing resources")
	progress := second.Resources[packResourceVectorMap]
	assert.Equal(t, progress.Total, progress.Reused)
	assert.Zero(t, progress.Downloaded)
	assert.Zero(t, progress.Revalidated)
	assert.EqualValues(t, 1, requests.Load(), "cached tiles must not cause network requests on retry")
	// Persisted progress must retain provenance across a process restart.
	raw, err := s.cache.root.ReadFile(filepath.Join(s.cache.packsRel, second.ID+".json"))
	require.NoError(t, err)
	var saved packManifest
	require.NoError(t, json.Unmarshal(raw, &saved))
	assert.Equal(t, progress, saved.Resources[packResourceVectorMap])
	assert.Equal(t, "region", saved.Input.CoverageKind)
	for _, key := range saved.CacheKeys {
		entry, ok := s.cache.get("maps-openfreemap", key)
		require.True(t, ok)
		if entry.Meta.ContentType == "application/x-protobuf" {
			entry.Meta.FreshUntil = time.Now().Add(-time.Minute)
			require.NoError(t, s.cache.updateMetadata(entry.Meta))
		}
	}
	third := run("Revalidate expired freshness")
	assert.EqualValues(t, 2, requests.Load())
	assert.Equal(t, 1, third.Resources[packResourceVectorMap].Revalidated)
	assert.Zero(t, third.Resources[packResourceVectorMap].Downloaded)
}
