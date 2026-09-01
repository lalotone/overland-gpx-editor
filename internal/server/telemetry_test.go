package server

import (
	"bytes"
	"context"
	"log/slog"
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
		StatsLogger:      slog.New(slog.NewTextHandler(&logs, nil)),
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
		if text := logs.String(); strings.Contains(text, "responses_hit=1") && strings.Contains(text, "packs_complete=1") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	text := logs.String()
	for _, wanted := range []string{"cache_get_hits=1", "cache_get_misses=1", "responses_hit=1", "responses_miss=1", "outbound_requests=1", "packs_complete=1"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("stats log does not contain %q:\n%s", wanted, text)
		}
	}
	sections := []string{
		"Offline stats: runtime",
		"Offline stats: cache storage",
		"Offline stats: cache responses",
		"Offline stats: outbound requests",
		"Offline stats: outbound latency",
		"Offline stats: packs",
		"Offline stats: elevation tiles",
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for _, section := range sections {
		found := false
		for _, line := range lines {
			if strings.Contains(line, section) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("stats log does not contain its own %q line:\n%s", section, text)
		}
	}
	for _, private := range []string{privateQuery, "private response", "private Pyrenees trip", "private-pack-id", upstream.URL} {
		if strings.Contains(text, private) {
			t.Errorf("stats log leaked %q:\n%s", private, text)
		}
	}
}

func TestNegativeStatsLogIntervalIsRejected(t *testing.T) {
	if _, err := New(Config{GPXDir: t.TempDir(), StatsLogInterval: -time.Second}); err == nil {
		t.Fatal("New accepted a negative stats log interval")
	}
}
