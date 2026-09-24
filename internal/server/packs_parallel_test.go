package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegionalPackHasSeparateBulkAdmission(t *testing.T) {
	var requests atomic.Int32
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/style.json" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"version":8,"sources":{"v":{"type":"vector","maxzoom":6,"tiles":[%q]}},"layers":[]}`, upstream.URL+"/vector/{z}/{x}/{y}.pbf")
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write([]byte("pbf"))
	}))
	t.Cleanup(upstream.Close)
	s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), OfflineCacheMaxBytes: 4 << 30, OfflineCacheMaxEntries: 200000, OpenFreeMapURL: upstream.URL + "/style.json", OpenFreeMapAllowBulk: true})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.cache.mu.Lock()
	s.cache.now = func() time.Time { return time.Unix(0, clock.Load()) }
	s.cache.mu.Unlock()
	var waits atomic.Int32
	s.cache.waitAdmission = func(ctx context.Context, delay time.Duration) error {
		waits.Add(1)
		clock.Add(int64(delay))
		return ctx.Err()
	}
	pack, _, err := s.packs.start(packInput{Regional: true, Name: "large map", BBox: &bbox{South: -70, West: -85, North: 70, East: 85}, ZoomMin: 6, ZoomMax: 6, Layers: []string{"openfreemap"}})
	require.NoError(t, err)
	var status packSummary
	require.Eventually(t, func() bool {
		status, _ = s.packs.publicManifest(pack.ID)
		return status.State == "complete"
	}, 90*time.Second, 10*time.Millisecond)
	assert.Greater(t, status.Total, 1000)
	assert.Zero(t, waits.Load(), "a bounded bulk pack must not hit the passive 1000-key ceiling")
	assert.Zero(t, status.Failures)
	assert.Empty(t, status.ErrorCode)
	assert.Equal(t, status.Total, status.Done)
	assert.EqualValues(t, status.Resources[packResourceVectorMap].Total, requests.Load(), "waiting must not fetch tiles twice")
}

func TestPackMapsProceedWhileElevationIsBlocked(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancel), func(t *testing.T) {
			elevationStarted := make(chan struct{}, 64)
			release := make(chan struct{})
			var active, peak atomic.Int32
			raw := encodeTerrarium(t, func(_, _ int) float64 { return 500 })
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/style.json":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"version":8,"sources":{"v":{"type":"vector","maxzoom":0,"tiles":[%q]}},"layers":[]}`, upstream.URL+"/vector/{z}/{x}/{y}.pbf")
				case strings.HasPrefix(r.URL.Path, "/vector/"):
					w.Header().Set("Content-Type", "application/x-protobuf")
					_, _ = w.Write([]byte("pbf"))
				case strings.HasPrefix(r.URL.Path, "/terrain/"):
					count := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); count > old && !peak.CompareAndSwap(old, count); old = peak.Load() {
					}
					elevationStarted <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write(raw)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(upstream.Close)
			s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), OfflineCacheMaxBytes: 4 << 30, OpenFreeMapURL: upstream.URL + "/style.json", OpenFreeMapAllowBulk: true, ElevationTiles: true, ElevationTileURL: upstream.URL + "/terrain/{z}/{x}/{y}.png", ElevationTileCache: t.TempDir()})
			require.NoError(t, err)
			cleanupTestServer(t, s)
			input := packInput{Regional: true, Name: "parallel", BBox: &bbox{South: 40, West: -1, North: 40.1, East: -0.9}, ZoomMin: 0, ZoomMax: 0, Layers: []string{"openfreemap"}, Scopes: []string{"elevation"}}
			pack, _, err := s.packs.startContext(t.Context(), input)
			require.NoError(t, err)
			for range 4 {
				select {
				case <-elevationStarted:
				case <-time.After(3 * time.Second):
					t.Fatal("elevation requests did not overlap")
				}
			}
			require.Eventually(t, func() bool {
				status, _ := s.packs.publicManifest(pack.ID)
				return status.Resources[packResourceVectorMap].Done == status.Resources[packResourceVectorMap].Total
			}, 3*time.Second, 10*time.Millisecond)
			status, _ := s.packs.publicManifest(pack.ID)
			assert.Zero(t, status.Resources[packResourceElevation].Done)
			assert.Equal(t, "running", status.State, "maps finishing must not complete the whole pack")
			if cancel {
				require.True(t, s.packs.cancel(pack.ID))
				status = waitForPackState(t, s.packs, pack.ID, "incomplete")
				assert.Equal(t, "cancelled", status.ErrorCode)
				require.Eventually(t, func() bool { return active.Load() == 0 }, 3*time.Second, 10*time.Millisecond)
			} else {
				close(release)
				status = waitForPackState(t, s.packs, pack.ID, "complete")
				assert.Equal(t, status.Total, status.Done)
				assert.Zero(t, status.Failures)
			}
			assert.LessOrEqual(t, peak.Load(), int32(4))
		})
	}
}
