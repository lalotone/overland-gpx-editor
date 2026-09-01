package server

import (
	"log/slog"
	"time"
)

type packOperationalStats struct {
	Total      int
	Queued     int
	Running    int
	Complete   int
	Incomplete int
}

func (m *packManager) operationalStats() packOperationalStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats := packOperationalStats{Total: len(m.packs)}
	for _, pack := range m.packs {
		switch pack.State {
		case "queued":
			stats.Queued++
		case "running":
			stats.Running++
		case "complete":
			stats.Complete++
		case "incomplete":
			stats.Incomplete++
		}
	}
	return stats
}

func (s *Server) logOperationalStatsLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.statsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.logOperationalStats(s.statsLogger)
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Server) logOperationalStats(logger *slog.Logger) {
	cache := s.cache.stats()
	outbound := s.outbound.statsSnapshot()
	packs := s.packs.operationalStats()
	var tiles tileCacheStats
	if s.elevation.tiles != nil {
		tiles = s.elevation.tiles.stats()
	}
	logger.Info("Offline stats: runtime",
		"mode", s.modes.mode(),
	)
	logger.Info("Offline stats: cache storage",
		"cache_writable", cache.Writable,
		"cache_entries", cache.Entries,
		"cache_bytes", cache.Bytes,
		"cache_quota_bytes", cache.Quota,
		"cache_reserved_bytes", cache.Reserved,
	)
	logger.Info("Offline stats: cache responses",
		"cache_get_hits", cache.Hits,
		"cache_get_misses", cache.Misses,
		"responses_hit", outbound.CacheHits,
		"responses_miss", outbound.CacheMisses,
		"responses_stale", outbound.CacheStale,
		"responses_revalidated", outbound.CacheRevalidated,
		"responses_bypass", outbound.CacheBypass,
	)
	logger.Info("Offline stats: outbound requests",
		"offline_misses", outbound.OfflineMisses+tiles.OfflineMisses,
		"outbound_requests", outbound.NetworkRequests+tiles.NetworkRequests,
		"outbound_failures", outbound.NetworkFailures+tiles.NetworkFailures,
		"outbound_status_2xx", outbound.Status2xx+tiles.NetworkStatus2xx,
		"outbound_status_3xx", outbound.Status3xx,
		"outbound_status_4xx", outbound.Status4xx+tiles.NetworkStatus4xx,
		"outbound_status_5xx", outbound.Status5xx+tiles.NetworkStatus5xx,
	)
	logger.Info("Offline stats: outbound latency",
		"outbound_avg_ms", averageDurationMS(outbound.Duration+tiles.NetworkDuration, outbound.NetworkRequests+tiles.NetworkRequests),
		"outbound_max_ms", max(outbound.MaxDuration, tiles.NetworkMaxDuration).Milliseconds(),
		"outbound_in_flight", outbound.InFlight+tiles.InFlight,
		"outbound_peak_in_flight", outbound.PeakInFlight,
		"outbound_queue_rejected", outbound.QueueRejected,
	)
	logger.Info("Offline stats: packs",
		"packs_total", packs.Total,
		"packs_queued", packs.Queued,
		"packs_running", packs.Running,
		"packs_complete", packs.Complete,
		"packs_incomplete", packs.Incomplete,
	)
	logger.Info("Offline stats: elevation tiles",
		"elevation_tile_memory_entries", tiles.MemoryEntries,
		"elevation_tile_disk_entries", tiles.DiskEntries,
		"elevation_tile_disk_bytes", tiles.DiskBytes,
		"elevation_tile_disk_quota_bytes", tiles.DiskQuota,
		"elevation_tile_memory_hits", tiles.MemoryHits,
		"elevation_tile_disk_hits", tiles.DiskHits,
		"elevation_tile_cache_misses", tiles.CacheMisses,
		"elevation_tile_shared_loads", tiles.SharedLoads,
	)
}

func averageDurationMS(duration time.Duration, count uint64) int64 {
	if count == 0 {
		return 0
	}
	return (duration / time.Duration(count)).Milliseconds()
}
