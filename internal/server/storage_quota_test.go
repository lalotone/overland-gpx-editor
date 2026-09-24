package server

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAvailableStorageQuotaKeepsMarginAndAvoidsOverflow(t *testing.T) {
	assert.Equal(t, int64(123), quotaWithFreeSpace(123, offlineDiskSafetyMargin-1))
	assert.Equal(t, int64(123+1024), quotaWithFreeSpace(123, offlineDiskSafetyMargin+1024))
	assert.Equal(t, int64(math.MaxInt64), quotaWithFreeSpace(123, math.MaxUint64))
}

func TestDeviceStoragePolicyReplacesFixedCacheBudgets(t *testing.T) {
	dir := t.TempDir()
	_, supported, err := availableDiskBytes(dir)
	require.NoError(t, err)
	if !supported {
		t.Skip("filesystem space reporting is unavailable on this platform")
	}
	s, err := New(Config{GPXDir: dir, OfflineCacheDir: t.TempDir(), UseAvailableStorage: true, OfflineCacheMaxBytes: 1, OfflineCacheMaxEntries: 200000, ElevationTiles: true, ElevationTileCache: t.TempDir(), ElevationTileCacheMaxBytes: 1})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	assert.Equal(t, int64(math.MaxInt64), s.cache.maxBytes)
	assert.Equal(t, int64(math.MaxInt64), s.elevation.tiles.maxDiskBytes)
	assert.True(t, s.cache.useAvailableStorage)
	assert.True(t, s.elevation.tiles.useAvailableStorage)
	assert.Less(t, s.cache.stats().Quota, int64(math.MaxInt64), "report actual filesystem capacity, not the internal sentinel")
	assert.Less(t, s.elevation.tiles.diskQuota(), int64(math.MaxInt64))
	assert.Equal(t, 200000, s.cache.scopeEntryLimit(), "mobile maps may use the whole bounded cache index")
	var storageErr cacheStorageLimitError
	require.ErrorAs(t, requirePackDiskSpace(dir, math.MaxInt64), &storageErr)
}
