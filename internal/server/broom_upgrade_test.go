package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoutingUpgradeLifecycle(t *testing.T) {
	for _, scenario := range []string{"startup", "startup prepare", "offline", "failure and retry", "pause and retry", "corrupt catalogue"} {
		t.Run(scenario, func(t *testing.T) {
			pbfPath, _ := writeBroomPBF(t)
			pbf, err := os.ReadFile(pbfPath)
			require.NoError(t, err)
			var dem bytes.Buffer
			zip := gzip.NewWriter(&dem)
			_, err = zip.Write(make([]byte, 1201*1201*2))
			require.NoError(t, err)
			require.NoError(t, zip.Close())
			var calls atomic.Int32
			var fail atomic.Bool
			var hold atomic.Bool
			entered := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if hold.Load() {
					select {
					case entered <- struct{}{}:
					default:
					}
					<-r.Context().Done()
					return
				}
				if fail.Load() {
					http.Error(w, "fixture unavailable", http.StatusBadRequest)
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, ".json"):
					_, _ = fmt.Fprint(w, `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"id":"fixture","name":"Fixture","parent":"parent","urls":{"pbf":"https://download.geofabrik.de/europe/fixture.osm.pbf"}},"geometry":{"type":"Polygon","coordinates":[[[1,42],[2,42],[2,43],[1,43],[1,42]]]}}]}`)
				case strings.HasSuffix(r.URL.Path, ".md5"):
					_, _ = fmt.Fprintf(w, "%x", md5.Sum(pbf))
				case strings.HasSuffix(r.URL.Path, ".pbf"):
					_, _ = w.Write(pbf)
				case strings.HasSuffix(r.URL.Path, ".hgt.gz"):
					_, _ = w.Write(dem.Bytes())
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(upstream.Close)
			cache := t.TempDir()
			provider := broom.ProviderOptions{IndexURL: upstream.URL + "/index.json", MetadataIndexURL: upstream.URL + "/metadata.json", PBFBaseURL: upstream.URL + "/pbf", DEMBaseURL: upstream.URL + "/dem"}
			manager, err := broom.New(broom.Options{CacheDir: cache, Provider: provider, BuildVersion: "previous-overland"})
			require.NoError(t, err)
			old, err := manager.SetupRegion(t.Context(), "fixture", broom.SetupOptions{Jobs: 1})
			require.NoError(t, err)
			require.NoError(t, old.Router.Close())
			// Emulate a previous build pipeline's catalogue descriptor. The graph
			// remains directly readable, as real v0.5.1 graphs do in v0.6.0.
			cataloguePath := filepath.Join(cache, "graphs", ".broom", "catalogue.json")
			contents, err := os.ReadFile(cataloguePath)
			require.NoError(t, err)
			var catalogue map[string]any
			require.NoError(t, json.Unmarshal(contents, &catalogue))
			entry := catalogue["Entries"].([]any)[0].(map[string]any)
			entry["PipelineVersion"] = 2
			entry["Build"].(map[string]any)["pipeline_version"] = 2
			contents, err = json.Marshal(catalogue)
			require.NoError(t, err)
			if scenario == "corrupt catalogue" {
				contents = []byte(`{"Version":`)
			}
			require.NoError(t, os.WriteFile(cataloguePath, contents, 0600))
			calls.Store(0)
			fail.Store(scenario == "failure and retry")
			hold.Store(scenario == "pause and retry")
			ctx, cancel := context.WithCancel(t.Context())
			var wg sync.WaitGroup
			modes := newOfflineModeController(ctx, modeAuto)
			if scenario == "offline" {
				_, err = modes.set(modeCacheOnly)
				require.NoError(t, err)
			}
			service, err := newBroomRoutingService(ctx, &wg, modes, broomRoutingConfig{
				CacheDir: cache, Region: "fixture", Jobs: 1,
				Prepare:  scenario == "startup prepare",
				IndexURL: provider.IndexURL, MetadataIndexURL: provider.MetadataIndexURL,
				PBFBaseURL: provider.PBFBaseURL, DEMBaseURL: provider.DEMBaseURL,
			}, upstream.Client())
			require.NoError(t, err)
			t.Cleanup(func() { cancel(); wg.Wait(); require.NoError(t, service.close()) })
			if scenario == "corrupt catalogue" {
				status := service.status(ctx)
				assert.False(t, status.Ready)
				assert.NotEmpty(t, status.Error)
				assert.Nil(t, status.Job)
				assert.Zero(t, calls.Load(), "corruption must not cause an implicit download")
				return
			}
			assert.True(t, service.status(ctx).Ready, "old graph must remain usable")
			if scenario == "offline" {
				status := service.status(ctx)
				assert.True(t, status.UpgradePending)
				assert.Nil(t, status.Job)
				assert.Zero(t, calls.Load())
				var request broomRouteRequest
				require.NoError(t, json.Unmarshal([]byte(`{"profile":"mixed","waypoints":[{"lat":42,"lon":1.00001},{"lat":42,"lon":1.00118}]}`), &request))
				route, err := service.route(ctx, request)
				require.NoError(t, err, "the previous graph must actually route offline")
				assert.Equal(t, old.Generation.GenerationID, route.GenerationID)
				assert.Zero(t, calls.Load())
				// Exercise the real mode-control hook, not just resumeUpgrade.
				server := &Server{broom: service, modes: modes}
				recorder := httptest.NewRecorder()
				server.handleOfflineMode(recorder, httptest.NewRequest(http.MethodPut, "/offline/mode", strings.NewReader(`{"mode":"auto"}`)))
				require.Equal(t, http.StatusOK, recorder.Code)
			}
			if scenario == "pause and retry" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("migration did not start acquisition")
				}
				require.True(t, service.cancelPreparation())
				require.Eventually(t, func() bool {
					status := service.status(ctx)
					return status.Job != nil && status.Job.State == "cancelled"
				}, 5*time.Second, 10*time.Millisecond)
				assert.True(t, service.status(ctx).Ready)
				hold.Store(false)
				require.NoError(t, service.startPreparation("fixture", false))
			}
			if scenario == "failure and retry" {
				require.Eventually(t, func() bool { status := service.status(ctx); return status.Job != nil && status.Job.State == "failed" }, 5*time.Second, 10*time.Millisecond)
				status := service.status(ctx)
				assert.True(t, status.Ready)
				assert.True(t, status.UpgradePending)
				assert.NotEmpty(t, status.Error)
				fail.Store(false)
				require.NoError(t, service.startPreparation("fixture", false), "ordinary preparation must retry the migration")
			}
			require.Eventually(t, func() bool { status := service.status(ctx); return status.Job != nil && status.Job.State == "complete" }, 10*time.Second, 10*time.Millisecond)
			status := service.status(ctx)
			assert.True(t, status.Ready)
			assert.True(t, status.Job.Upgrading)
			assert.False(t, status.UpgradePending)
			assert.Empty(t, status.Error)
			assert.NotEqual(t, old.Generation.GenerationID, status.GenerationID)
			ready, err := manager.OpenRegion(ctx, "fixture", broom.OpenRegionOptions{})
			require.NoError(t, err)
			assert.EqualValues(t, 3, ready.Generation.PipelineVersion)
			require.NoError(t, ready.Router.Close())
		})
	}
}
