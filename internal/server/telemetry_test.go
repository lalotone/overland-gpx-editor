package server

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedLogBuffer) Write(raw []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(raw)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestOperationalStatsArePeriodicAndPrivate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = w.Write([]byte(`[{"display_name":"private response"}]`))
	}))
	defer upstream.Close()

	var logs lockedLogBuffer
	s, err := New(Config{
		GPXDir:           t.TempDir(),
		OfflineCacheDir:  t.TempDir(),
		NominatimURL:     upstream.URL,
		StatsLogInterval: 5 * time.Millisecond,
		StatsLogger:      log.New(&logs, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cleanupTestServer(t, s)

	const privateQuery = "hidden-ridge-near-41.1234,-1.9876"
	request := cachedRequest{
		policy: s.providers["places"], method: http.MethodGet,
		url: upstream.URL + "/search?q=" + privateQuery, params: "q=" + privateQuery,
		cacheable: true,
	}
	if response, err := s.outbound.do(context.Background(), request); err != nil || response.State != "miss" {
		t.Fatalf("initial request = state %q, err %v", response.State, err)
	}
	if response, err := s.outbound.do(context.Background(), request); err != nil || response.State != "hit" {
		t.Fatalf("cached request = state %q, err %v", response.State, err)
	}
	s.packs.mu.Lock()
	s.packs.packs["private-pack-id"] = &packManifest{Name: "private Pyrenees trip", State: "complete"}
	s.packs.mu.Unlock()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if text := logs.String(); strings.Contains(text, "1 hit / 1 miss") && strings.Contains(text, "1 complete") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	text := logs.String()
	for _, wanted := range []string{
		"OFFLINE STATISTICS (cumulative) - mode: auto",
		"SECTION          METRIC            VALUE",
		"Cache storage    Persistence       writable",
		"Cache responses  Store gets        1 hit / 1 miss (50.0% hit rate)",
		"Request outcomes  1 hit / 1 miss / 0 stale / 0 revalidated / 0 bypasses",
		"Outbound         Requests          1 request / 0 failures / 0 offline misses",
		"Packs            Total             1 pack",
		"States            0 queued / 0 running / 1 complete / 0 incomplete",
		"Elevation tiles  Status            disabled",
	} {
		if !strings.Contains(text, wanted) {
			t.Errorf("stats log does not contain %q:\n%s", wanted, text)
		}
	}
	for _, garbage := range []string{"cache_get_hits=", "responses_hit=", "packs_complete=", "level=INFO", `msg="`} {
		if strings.Contains(text, garbage) {
			t.Errorf("stats table contains structured-log garbage %q:\n%s", garbage, text)
		}
	}
	for _, private := range []string{privateQuery, "private response", "private Pyrenees trip", "private-pack-id", upstream.URL} {
		if strings.Contains(text, private) {
			t.Errorf("stats log leaked %q:\n%s", private, text)
		}
	}
}

func TestOperationalStatsFormatting(t *testing.T) {
	report := formatOperationalStats(
		modeCacheOnly,
		cacheStats{Writable: true, Entries: 1234, Bytes: 256 << 20, Quota: 1 << 30, Reserved: 16 << 20, Hits: 9, Misses: 1},
		outboundStats{
			CacheHits: 8, CacheMisses: 2, CacheStale: 1, CacheRevalidated: 3, CacheBypass: 4,
			OfflineMisses: 5, QueueRejected: 3, NetworkRequests: 12, NetworkFailures: 2,
			Status2xx: 9, Status3xx: 1, Status4xx: 1, Status5xx: 1,
			Duration: 240 * time.Millisecond, MaxDuration: 80 * time.Millisecond, InFlight: 2, PeakInFlight: 4,
		},
		packOperationalStats{Total: 4, Queued: 1, Running: 1, Complete: 1, Incomplete: 1},
		tileCacheStats{
			MemoryEntries: 5, DiskEntries: 6, DiskBytes: 64 << 20, DiskQuota: 512 << 20, InFlight: 1,
			MemoryHits: 7, DiskHits: 8, CacheMisses: 9, SharedLoads: 10, OfflineMisses: 2,
			NetworkRequests: 1, NetworkFailures: 1, NetworkStatus2xx: 1,
			NetworkDuration: 10 * time.Millisecond, NetworkMaxDuration: 20 * time.Millisecond,
		},
		true,
		true,
	)

	for _, wanted := range []string{
		"OFFLINE STATISTICS (cumulative) - mode: cache-only",
		"Entries           1,234 entries",
		"Usage             256 MiB / 1.0 GiB (25.0%)",
		"Store gets        9 hits / 1 miss (90.0% hit rate)",
		"Requests          13 requests / 3 failures / 7 offline misses",
		"HTTP status       10 2xx / 1 3xx / 1 4xx / 1 5xx",
		"Latency           19 ms avg / 80 ms max",
		"Concurrency       2 current / 4 peak",
		"In flight         1",
		"Disk              6 entries / 64.0 MiB / 512 MiB (12.5%) / 8 hits",
		"Cache             9 misses / 10 shared loads",
	} {
		if !strings.Contains(report, wanted) {
			t.Errorf("stats table does not contain %q:\n%s", wanted, report)
		}
	}
}

func TestNegativeStatsLogIntervalIsRejected(t *testing.T) {
	if _, err := New(Config{GPXDir: t.TempDir(), StatsLogInterval: -time.Second}); err == nil {
		t.Fatal("New accepted a negative stats log interval")
	}
}
