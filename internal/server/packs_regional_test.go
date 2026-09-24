package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalunaRegionalPackExceedsOrdinaryLimitSafely(t *testing.T) {
	upstream, _ := newFakeMapSource(t)
	s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), OfflineCacheMaxBytes: 4 << 30, OfflineCacheMaxEntries: 200000, ElevationTiles: true, ElevationTileCache: t.TempDir(), ElevationTileCacheMaxBytes: 2 << 30, OpenFreeMapURL: upstream.URL, OpenFreeMapAllowBulk: true})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	input := packInput{Name: "Cataluña", BBox: &bbox{South: 40.52, West: 0.15, North: 42.87, East: 3.33}, ZoomMin: 5, ZoomMax: 14, Layers: []string{"openfreemap"}, Scopes: []string{"elevation", "fuel", "pois"}}
	_, err = s.packs.estimateContext(t.Context(), input)
	require.ErrorContains(t, err, "resources")
	input.Regional = true
	estimate, err := s.packs.estimateContext(t.Context(), input)
	require.NoError(t, err)
	assert.Greater(t, estimate.Resources, maxPackResources)
	assert.LessOrEqual(t, estimate.Resources, maxRegionalResources)
	assert.Less(t, estimate.GenericBytes, estimate.RemainingQuota)
	assert.Greater(t, len(estimate.ElevationTiles), maxElevationPackEntries)
	assert.Empty(t, estimate.POIRequests, "a region must not become tiled Overpass harvesting")
	assert.Len(t, estimate.Blocked, 3, "unsupported broad place requests must be explicit")
	t.Logf("Cataluña: %d resources, %d terrain tiles, %.1f MiB estimated", estimate.Resources, len(estimate.ElevationTiles), float64(estimate.EstimatedBytes)/(1<<20))
}

func TestRegionalTileBatchesBoundConcurrency(t *testing.T) {
	keys := make([]tileKey, regionalBatchSize*2+5)
	for i := range keys {
		keys[i] = tileKey{z: 10, x: i, y: 0}
	}
	var active, peak, completed atomic.Int32
	ok := runTileBatches(t.Context(), keys, true, func(ctx context.Context, key tileKey) bool {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
		}
		if key.x >= regionalBatchSize {
			assert.GreaterOrEqual(t, completed.Load(), int32(key.x/regionalBatchSize*regionalBatchSize))
		}
		time.Sleep(time.Millisecond)
		completed.Add(1)
		return ctx.Err() == nil
	})
	assert.True(t, ok)
	assert.EqualValues(t, len(keys), completed.Load())
	assert.LessOrEqual(t, peak.Load(), int32(regionalWorkers))
	assert.False(t, runTileBatches(t.Context(), keys, true, func(context.Context, tileKey) bool { return false }))
}

func TestRegionalManifestCheckpointsAndFinalFlush(t *testing.T) {
	s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), OfflineCacheMaxBytes: 4 << 30})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	id, err := newPackID()
	require.NoError(t, err)
	p := &packManifest{ID: id, Name: "Region", State: "running", Total: 50, Input: packInput{Regional: true}, Resources: map[string]packResourceProgress{}}
	require.NoError(t, s.packs.persistLocked(p))
	p.lastCheckpoint = time.Now().Add(time.Hour)
	for range 50 {
		require.True(t, s.packs.update(p, func(p *packManifest) { p.Done++ }))
	}
	read := func() packManifest {
		data, err := s.cache.root.ReadFile(filepath.Join(s.cache.packsRel, id+".json"))
		require.NoError(t, err)
		var saved packManifest
		require.NoError(t, json.Unmarshal(data, &saved))
		return saved
	}
	assert.Zero(t, read().Done, "individual resources should not rewrite the growing regional manifest")
	require.True(t, s.packs.update(p, func(p *packManifest) { p.State = "complete" }))
	assert.Equal(t, 50, read().Done, "terminal states must always be durable")
	assert.Equal(t, "complete", read().State)
}

func TestRegionalVectorPackCompletesAndRestoresAllBatchPins(t *testing.T) {
	upstream, calls := newFakeMapSource(t)
	cfg := Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), OfflineCacheMaxBytes: 4 << 30, OpenFreeMapURL: upstream.URL, OpenFreeMapAllowBulk: true}
	s, err := New(cfg)
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = s.Close()
		}
	})
	input := packInput{Regional: true, Name: "Batched region", BBox: &bbox{South: -20, West: -80, North: 60, East: 80}, ZoomMin: 5, ZoomMax: 5, Layers: []string{"openfreemap"}}
	pack, _, err := s.packs.startContext(t.Context(), input)
	require.NoError(t, err)
	var summary packSummary
	require.Eventually(t, func() bool {
		summary, _ = s.packs.publicManifest(pack.ID)
		return summary.State == "complete"
	}, 30*time.Second, 10*time.Millisecond)
	assert.Greater(t, summary.BatchesTotal, 1)
	assert.Equal(t, summary.BatchesTotal, summary.BatchesDone)
	assert.Equal(t, summary.Total, summary.Done)
	require.NoError(t, s.Close())
	closed = true
	before := calls.Load()
	cfg.OfflineMode = "cache-only"
	reopened, err := New(cfg)
	require.NoError(t, err)
	cleanupTestServer(t, reopened)
	restored := reopened.packs.packs[pack.ID]
	require.NotNil(t, restored)
	assert.Equal(t, "complete", restored.State)
	assert.Equal(t, summary.Total, len(restored.CacheKeys))
	for _, key := range restored.CacheKeys {
		entry, ok := reopened.cache.get("maps-openfreemap", key)
		require.True(t, ok)
		assert.Contains(t, entry.Meta.Pins, pack.ID)
	}
	assert.Equal(t, before, calls.Load(), "reopening must be entirely local")
}

func TestTerrainPinsPreventEvictionAndSurviveRestart(t *testing.T) {
	ts := newTileServer(t)
	raw := encodeTerrarium(t, func(_, _ int) float64 { return 500 })
	cfg := Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), ElevationTiles: true, ElevationTileCache: t.TempDir(), ElevationTileCacheMaxBytes: int64(len(raw) * 2), ElevationTileURL: ts.url(), HTTPClient: ts.Client()}
	s, err := New(cfg)
	require.NoError(t, err)
	keys := []tileKey{{z: defaultTileZoom, x: 1, y: 1}, {z: defaultTileZoom, x: 1, y: 2}}
	for _, key := range keys {
		_, err := s.elevation.tiles.grid(t.Context(), key)
		require.NoError(t, err)
	}
	id, err := newPackID()
	require.NoError(t, err)
	p := &packManifest{ID: id, Name: "Pinned terrain", State: "complete", Resources: map[string]packResourceProgress{packResourceElevation: {Done: 2, Total: 2}}, ElevationKeys: []string{keys[0].path(), keys[1].path()}}
	s.elevation.tiles.setPackPins(id, p.ElevationKeys)
	require.NoError(t, s.packs.persistLocked(p))
	_, err = s.elevation.tiles.grid(t.Context(), tileKey{z: defaultTileZoom, x: 1, y: 3})
	require.Error(t, err, "a later tile must not evict the pack's earlier tiles")
	require.NoError(t, s.Close())
	cfg.ElevationTileCacheMaxBytes = int64(len(raw))
	cfg.OfflineMode = "cache-only"
	restarted, err := New(cfg)
	require.NoError(t, err)
	cleanupTestServer(t, restarted)
	for _, key := range keys {
		_, err := restarted.elevation.tiles.grid(t.Context(), key)
		require.NoError(t, err, "pins must be restored before startup eviction")
	}
	assert.Equal(t, "complete", restarted.packs.packs[id].State)
}
