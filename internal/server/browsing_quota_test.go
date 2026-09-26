package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// countDownloadsAgainstQuota simulates a platform that cannot measure free
// space, where downloads keep counting against the byte quotas.
func countDownloadsAgainstQuota(t *testing.T) {
	t.Helper()
	previous := diskSpaceMeasurable
	diskSpaceMeasurable = func(string) bool { return false }
	t.Cleanup(func() { diskSpaceMeasurable = previous })
}

func browsingEntry(i int, now time.Time) cacheMetadata {
	return cacheMetadata{Key: fmt.Sprintf("%064x", i), Scope: "maps-openfreemap", Status: 200, FetchedAt: now, FreshUntil: now.Add(time.Hour), StaleUntil: now.Add(24 * time.Hour)}
}

func TestByteQuotaLimitsBrowsingNotDownloads(t *testing.T) {
	s, err := newCacheStore(t.TempDir(), 64<<10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	s.downloadsUseDisk = true
	now := time.Now().UTC()
	body := []byte(strings.Repeat("x", 10<<10))
	budget := func(int64) error { return nil }

	// Browsing data first, close to the 64 KiB quota.
	for i := 200; i < 205; i++ {
		if err := s.put(browsingEntry(i, now.Add(-time.Hour)), body); err != nil {
			t.Fatal(err)
		}
	}
	// Twelve downloaded entries: ~120 KiB, twice the quota. They must neither
	// be refused nor push out what was cached while browsing.
	for i := 0; i < 12; i++ {
		meta := browsingEntry(i, now)
		if _, err := s.putWithAdmission(meta, body, budget); err != nil {
			t.Fatalf("download %d refused: %v", i, err)
		}
		if err := s.pin(meta.Key, "pack", true); err != nil {
			t.Fatal(err)
		}
	}
	for i := 200; i < 205; i++ {
		if _, ok := s.get("maps-openfreemap", browsingEntry(i, now).Key); !ok {
			t.Fatalf("a download evicted browsing entry %d", i)
		}
	}
	// Browsing keeps working, and only browsing data is evicted to fit.
	for i := 100; i < 120; i++ {
		if err := s.put(browsingEntry(i, now.Add(time.Duration(i)*time.Second)), body); err != nil {
			t.Fatalf("browsing entry %d refused: %v", i, err)
		}
	}
	s.mu.Lock()
	charged, pinned := s.chargedBytesLocked(), s.pinnedBytes
	s.mu.Unlock()
	if charged > 64<<10 {
		t.Fatalf("browsing data %d exceeds the 64 KiB quota", charged)
	}
	if pinned < 12*int64(len(body)) {
		t.Fatalf("downloads were evicted: %d pinned bytes remain", pinned)
	}
	for i := 0; i < 12; i++ {
		if _, ok := s.get("maps-openfreemap", browsingEntry(i, now).Key); !ok {
			t.Fatalf("downloaded entry %d was evicted", i)
		}
	}

	// Releasing downloads returns them to browsing, trimmed back under quota.
	for i := 0; i < 12; i++ {
		if err := s.pin(browsingEntry(i, now).Key, "pack", false); err != nil {
			t.Fatalf("unpinning %d failed: %v", i, err)
		}
	}
	s.mu.Lock()
	charged, pinned = s.chargedBytesLocked(), s.pinnedBytes
	s.mu.Unlock()
	if charged > 64<<10 || pinned != 0 {
		t.Fatalf("after release: browsing %d, pinned %d", charged, pinned)
	}
}

func TestPerScopeShareCountsOnlyBrowsingEntries(t *testing.T) {
	s, err := newCacheStore(t.TempDir(), 1<<30, 40)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	s.downloadsUseDisk = true
	now := time.Now().UTC()
	// Fill the per-scope share (maxEntries/4 = 10) with browsing entries.
	for i := 100; i < 110; i++ {
		if err := s.put(browsingEntry(i, now.Add(-time.Hour)), []byte("tile")); err != nil {
			t.Fatal(err)
		}
	}
	// A download may exceed the share without displacing browsing entries.
	for i := 0; i < 20; i++ {
		meta := browsingEntry(i, now)
		if _, err := s.putWithAdmission(meta, []byte("tile"), func(int64) error { return nil }); err != nil {
			t.Fatalf("download %d refused by the scope share: %v", i, err)
		}
		if err := s.pin(meta.Key, "pack", true); err != nil {
			t.Fatal(err)
		}
	}
	for i := 100; i < 110; i++ {
		if _, ok := s.get("maps-openfreemap", browsingEntry(i, now).Key); !ok {
			t.Fatalf("a download displaced browsing entry %d", i)
		}
	}
	if err := s.put(browsingEntry(99, now), []byte("tile")); err != nil {
		t.Fatalf("browsing entry refused: %v", err)
	}
}

func TestDownloadsIgnoreBrowsingQuotaEndToEnd(t *testing.T) {
	s := fuelPackServerWith(t, Config{OfflineCacheMaxBytes: 1 << 20})
	if !s.cache.downloadsUseDisk {
		t.Skip("free disk space is not measurable on this platform")
	}
	// The manifest reservation alone exceeds 1 MiB; downloads must not care.
	pack, _, err := s.packs.start(packInput{Name: "fuel", BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"fuel"}})
	if err != nil {
		t.Fatal(err)
	}
	waitForPackState(t, s.packs, pack.ID, "complete")
}

func TestPackTilesIgnoreBrowsingTileQuota(t *testing.T) {
	ts := newTileServer(t)
	raw := encodeTerrarium(t, func(_, _ int) float64 { return 500 })
	s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), ElevationTiles: true, ElevationTileCache: t.TempDir(), ElevationTileCacheMaxBytes: int64(len(raw) * 2), ElevationTileURL: ts.url(), HTTPClient: ts.Client()})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	tiles := s.elevation.tiles
	if !tiles.downloadsUseDisk {
		t.Skip("free disk space is not measurable on this platform")
	}
	pinned := []tileKey{{z: defaultTileZoom, x: 1, y: 1}, {z: defaultTileZoom, x: 1, y: 2}, {z: defaultTileZoom, x: 1, y: 3}, {z: defaultTileZoom, x: 1, y: 4}}
	paths := make([]string, len(pinned))
	for i, key := range pinned {
		paths[i] = key.path()
	}
	// Browsing tiles fill the two-tile quota before the pack starts.
	browsing := []tileKey{{z: defaultTileZoom, x: 3, y: 1}, {z: defaultTileZoom, x: 3, y: 2}}
	for _, key := range browsing {
		if _, err := tiles.grid(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
	tiles.setPackPins("pack", paths)
	for _, key := range pinned {
		if _, err := tiles.grid(t.Context(), key); err != nil {
			t.Fatalf("pack tile refused by the browsing quota: %v", err)
		}
	}
	for _, key := range browsing {
		if _, ok := tiles.diskFileSize(key); !ok {
			t.Fatalf("a pack tile evicted browsing tile %v", key)
		}
	}
	for y := 10; y < 14; y++ {
		if _, err := tiles.grid(t.Context(), tileKey{z: defaultTileZoom, x: 2, y: y}); err != nil {
			t.Fatalf("browsing tile refused: %v", err)
		}
	}
	tiles.diskMu.Lock()
	charged := tiles.chargedDiskBytesLocked()
	tiles.diskMu.Unlock()
	if charged > int64(len(raw)*2) {
		t.Fatalf("browsing tiles use %d bytes over a %d quota", charged, len(raw)*2)
	}
	for _, key := range pinned {
		if _, ok := tiles.diskFileSize(key); !ok {
			t.Fatalf("pack tile %v was evicted", key)
		}
	}
	tiles.releasePackPins("pack")
	tiles.diskMu.Lock()
	charged = tiles.chargedDiskBytesLocked()
	tiles.diskMu.Unlock()
	if charged > int64(len(raw)*2) {
		t.Fatalf("released tiles were not trimmed: %d bytes", charged)
	}
}

func TestRefreshingADownloadedEntryKeepsBrowsingData(t *testing.T) {
	s, err := newCacheStore(t.TempDir(), 32<<10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	s.downloadsUseDisk = true
	now := time.Now().UTC()
	body := []byte(strings.Repeat("x", 10<<10))
	downloaded := browsingEntry(1, now)
	if _, err := s.putWithAdmission(downloaded, body, func(int64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.pin(downloaded.Key, "pack", true); err != nil {
		t.Fatal(err)
	}
	for i := 10; i < 12; i++ {
		if err := s.put(browsingEntry(i, now.Add(-time.Hour)), body); err != nil {
			t.Fatal(err)
		}
	}
	// A browsing request revalidates the downloaded tile with a larger body.
	if err := s.put(browsingEntry(1, now.Add(time.Minute)), []byte(strings.Repeat("y", 12<<10))); err != nil {
		t.Fatal(err)
	}
	for i := 10; i < 12; i++ {
		if _, ok := s.get("maps-openfreemap", browsingEntry(i, now).Key); !ok {
			t.Fatalf("refreshing a downloaded entry evicted browsing entry %d", i)
		}
	}
	if entry, ok := s.get("maps-openfreemap", downloaded.Key); !ok || len(entry.Meta.Pins) != 1 {
		t.Fatalf("refreshed download lost its pin: %+v", entry)
	}
}
