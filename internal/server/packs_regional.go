package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxRegionalResources     = 100000
	maxRegionalManifestBytes = 8 << 20
	regionalBatchSize        = 128
	regionalWorkers          = 2
	regionalJobTimeout       = 6 * time.Hour
)

func packResourceLimit(input packInput) int {
	if input.Regional {
		return maxRegionalResources
	}
	return maxPackResources
}

func packManifestLimit(input packInput) int {
	if input.Regional {
		return maxRegionalManifestBytes
	}
	return maxPackManifestBytes
}

func packElevationLimits(input packInput) (int, int64) {
	if input.Regional {
		return 16384, 2 << 30
	}
	return maxElevationPackEntries, maxElevationPackBytes
}

func (m *packManager) regionalControlReserve() int64 {
	reserve := int64(maxStoredPackManifests * maxRegionalManifestBytes)
	if m.server.openFreeMap != nil {
		reserve += maxMapGenerationBytes
	}
	return reserve
}

// One regional manifest owns all pins. Processing bounded batches avoids the
// ordinary per-pack tile limit without creating dozens of evictable child packs.
// Batch workers share the existing provider queue, admission budget and progress.
func runTileBatches(ctx context.Context, keys []tileKey, regional bool, fetch func(context.Context, tileKey) bool) bool {
	if !regional {
		for _, key := range keys {
			if !fetch(ctx, key) {
				return false
			}
		}
		return true
	}
	for start := 0; start < len(keys); start += regionalBatchSize {
		if ctx.Err() != nil {
			return false
		}
		batchCtx, cancel := context.WithCancel(ctx)
		end := min(start+regionalBatchSize, len(keys))
		var next atomic.Int64
		var failed atomic.Bool
		var workers sync.WaitGroup
		for range regionalWorkers {
			workers.Go(func() {
				for batchCtx.Err() == nil {
					index := start + int(next.Add(1)) - 1
					if index >= end {
						return
					}
					if !fetch(batchCtx, keys[index]) {
						failed.Store(true)
						cancel()
						return
					}
				}
			})
		}
		workers.Wait()
		cancel()
		if failed.Load() || ctx.Err() != nil {
			return false
		}
	}
	return true
}

// Called under packManager.mu; a set keeps whole-region pin bookkeeping linear.
func (p *packManifest) addCacheKey(key string) {
	if p.cacheKeySet == nil {
		p.cacheKeySet = make(map[string]struct{}, len(p.CacheKeys)+1)
		for _, existing := range p.CacheKeys {
			p.cacheKeySet[existing] = struct{}{}
		}
	}
	if _, exists := p.cacheKeySet[key]; !exists {
		p.cacheKeySet[key] = struct{}{}
		p.CacheKeys = append(p.CacheKeys, key)
	}
}
