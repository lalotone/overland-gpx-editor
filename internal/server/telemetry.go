package server

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"text/tabwriter"
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

func (s *Server) logOperationalStats(logger *log.Logger) {
	cache := s.cache.stats()
	outbound := s.outbound.statsSnapshot()
	packs := s.packs.operationalStats()
	var tiles tileCacheStats
	tilesEnabled := s.elevation.tiles != nil
	tileDiskEnabled := tilesEnabled && s.elevation.tiles.cacheDir != ""
	if s.elevation.tiles != nil {
		tiles = s.elevation.tiles.stats()
	}
	logger.Print(formatOperationalStats(s.modes.mode(), cache, outbound, packs, tiles, tilesEnabled, tileDiskEnabled))
}

func formatOperationalStats(mode offlineMode, cache cacheStats, outbound outboundStats, packs packOperationalStats, tiles tileCacheStats, tilesEnabled, tileDiskEnabled bool) string {
	var report strings.Builder
	fmt.Fprintf(&report, "OFFLINE STATISTICS (cumulative) - mode: %s\n", mode)
	table := tabwriter.NewWriter(&report, 0, 4, 2, ' ', 0)
	row := func(section, metric, value string) {
		fmt.Fprintf(table, "%s\t%s\t%s\n", section, metric, value)
	}
	row("SECTION", "METRIC", "VALUE")
	row("-------", "------", "-----")
	persistence := "disabled"
	if cache.Writable {
		persistence = "writable"
	}
	row("Cache storage", "Persistence", persistence)
	row("", "Entries", counted(uint64(cache.Entries), "entry", "entries"))
	row("", "Usage", formatUsage(cache.Bytes, cache.Quota))
	row("", "Reserved", formatBytes(cache.Reserved))
	row("Cache responses", "Store gets", fmt.Sprintf("%s / %s (%s hit rate)", counted(cache.Hits, "hit", "hits"), counted(cache.Misses, "miss", "misses"), formatRate(cache.Hits, cache.Hits+cache.Misses)))
	row("", "Request outcomes", fmt.Sprintf("%s / %s / %s / %s / %s",
		counted(outbound.CacheHits, "hit", "hits"), counted(outbound.CacheMisses, "miss", "misses"),
		counted(outbound.CacheStale, "stale", "stale"), counted(outbound.CacheRevalidated, "revalidated", "revalidated"), counted(outbound.CacheBypass, "bypass", "bypasses")))
	networkRequests := outbound.NetworkRequests + tiles.NetworkRequests
	row("Outbound", "Requests", fmt.Sprintf("%s / %s / %s",
		counted(networkRequests, "request", "requests"),
		counted(outbound.NetworkFailures+tiles.NetworkFailures, "failure", "failures"),
		counted(outbound.OfflineMisses+tiles.OfflineMisses, "offline miss", "offline misses")))
	row("", "HTTP status", fmt.Sprintf("%s 2xx / %s 3xx / %s 4xx / %s 5xx",
		formatCount(outbound.Status2xx+tiles.NetworkStatus2xx), formatCount(outbound.Status3xx),
		formatCount(outbound.Status4xx+tiles.NetworkStatus4xx), formatCount(outbound.Status5xx+tiles.NetworkStatus5xx)))
	row("", "Latency", fmt.Sprintf("%s avg / %s max",
		formatMilliseconds(averageDurationMS(outbound.Duration+tiles.NetworkDuration, networkRequests)),
		formatMilliseconds(max(outbound.MaxDuration, tiles.NetworkMaxDuration).Milliseconds())))
	row("", "Concurrency", fmt.Sprintf("%s current / %s peak", formatCount(uint64(outbound.InFlight)), formatCount(uint64(outbound.PeakInFlight))))
	row("", "Queue rejected", counted(outbound.QueueRejected, "request", "requests"))
	row("Packs", "Total", counted(uint64(packs.Total), "pack", "packs"))
	row("", "States", fmt.Sprintf("%s / %s / %s / %s",
		counted(uint64(packs.Queued), "queued", "queued"), counted(uint64(packs.Running), "running", "running"),
		counted(uint64(packs.Complete), "complete", "complete"), counted(uint64(packs.Incomplete), "incomplete", "incomplete")))
	if !tilesEnabled {
		row("Elevation tiles", "Status", "disabled")
	} else {
		row("Elevation tiles", "Memory", fmt.Sprintf("%s / %s", counted(uint64(tiles.MemoryEntries), "entry", "entries"), counted(tiles.MemoryHits, "hit", "hits")))
		row("", "In flight", formatCount(uint64(tiles.InFlight)))
		if tileDiskEnabled {
			row("", "Disk", fmt.Sprintf("%s / %s / %s", counted(uint64(tiles.DiskEntries), "entry", "entries"), formatUsage(tiles.DiskBytes, tiles.DiskQuota), counted(tiles.DiskHits, "hit", "hits")))
		} else {
			row("", "Disk", "disabled")
		}
		row("", "Cache", fmt.Sprintf("%s / %s", counted(tiles.CacheMisses, "miss", "misses"), counted(tiles.SharedLoads, "shared load", "shared loads")))
	}
	_ = table.Flush()
	return strings.TrimRight(report.String(), "\n")
}

func counted(value uint64, singular, plural string) string {
	label := plural
	if value == 1 {
		label = singular
	}
	return formatCount(value) + " " + label
}

func formatCount(value uint64) string {
	raw := strconv.FormatUint(value, 10)
	firstGroup := len(raw) % 3
	if firstGroup == 0 {
		firstGroup = 3
	}
	var formatted strings.Builder
	formatted.WriteString(raw[:firstGroup])
	for offset := firstGroup; offset < len(raw); offset += 3 {
		formatted.WriteByte(',')
		formatted.WriteString(raw[offset : offset+3])
	}
	return formatted.String()
}

func formatBytes(value int64) string {
	if value <= 0 {
		return "0 B"
	}
	units := [...]string{"B", "KiB", "MiB", "GiB", "TiB"}
	size := float64(value)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	if unit == 0 {
		return counted(uint64(value), "B", "B")
	}
	precision := 1
	if size >= 100 {
		precision = 0
	}
	return fmt.Sprintf("%.*f %s", precision, size, units[unit])
}

func formatUsage(used, quota int64) string {
	if quota <= 0 {
		return formatBytes(used) + " / no quota"
	}
	return fmt.Sprintf("%s / %s (%.1f%%)", formatBytes(used), formatBytes(quota), float64(max(int64(0), used))*100/float64(quota))
}

func formatRate(value, total uint64) string {
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(value)*100/float64(total))
}

func formatMilliseconds(value int64) string {
	return formatCount(uint64(max(int64(0), value))) + " ms"
}

func averageDurationMS(duration time.Duration, count uint64) int64 {
	if count == 0 {
		return 0
	}
	return (duration / time.Duration(count)).Milliseconds()
}
