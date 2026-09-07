package overland

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lalotone/overland-gpx-editor/web"
)

func TestNewServesFrontendAndAPI(t *testing.T) {
	app, err := New(Config{
		GPXDir:      t.TempDir(),
		OfflineMode: "cache-only",
		Assets: fstest.MapFS{
			"index.html": {Data: []byte("<!doctype html><head></head><h1>Overland</h1>")},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", recorder.Code, http.StatusOK)
	}
	for _, fragment := range []string{
		"<h1>Overland</h1>",
		`name="gpx-editor-offline-mode" content="cache-only"`,
	} {
		if !strings.Contains(recorder.Body.String(), fragment) {
			t.Errorf("GET / body does not contain %q: %s", fragment, recorder.Body.String())
		}
	}

	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusNoContent {
		t.Errorf("GET /healthz status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestDefaultConfigEnablesCurrentBackendCapabilities(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "data")
	cacheHome := filepath.Join(t.TempDir(), "cache")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	cfg := DefaultConfig()
	if got, want := cfg.GPXDir, filepath.Join(dataHome, "overland", "gpx"); got != want {
		t.Errorf("GPXDir = %q, want %q", got, want)
	}
	if got, want := cfg.ElevationTileCache, filepath.Join(cacheHome, "overland", "tiles"); got != want {
		t.Errorf("ElevationTileCache = %q, want %q", got, want)
	}
	if got, want := cfg.OfflineCacheDir, filepath.Join(cacheHome, "overland", "responses"); got != want {
		t.Errorf("OfflineCacheDir = %q, want %q", got, want)
	}
	if !cfg.ElevationTiles {
		t.Error("ElevationTiles = false, want true")
	}
	if cfg.OpenFreeMapURL == "" || !cfg.OpenFreeMapAllowBulk {
		t.Errorf("OpenFreeMap defaults = (%q, %t), want enabled with bulk fetching", cfg.OpenFreeMapURL, cfg.OpenFreeMapAllowBulk)
	}

	// Cache-only exercises the advertised desktop capabilities without making
	// startup contact the configured public map source.
	cfg.OfflineMode = "cache-only"
	cfg.Assets = fstest.MapFS{"index.html": {Data: []byte("<head></head>")}}
	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New(DefaultConfig()) error = %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /config status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response struct {
		Offline struct {
			Mode        string `json:"mode"`
			ModeControl string `json:"modeControl"`
		} `json:"offline"`
		Maps struct {
			OpenFreeMap *struct {
				Style     string `json:"style"`
				AllowBulk bool   `json:"allowBulk"`
			} `json:"openfreemap"`
		} `json:"maps"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode /config: %v", err)
	}
	if response.Offline.Mode != "cache-only" || response.Offline.ModeControl != "" {
		t.Errorf("offline config = (%q, %q), want locked cache-only mode", response.Offline.Mode, response.Offline.ModeControl)
	}
	if response.Maps.OpenFreeMap == nil || !response.Maps.OpenFreeMap.AllowBulk {
		t.Fatalf("OpenFreeMap capability = %#v, want bulk-enabled proxy", response.Maps.OpenFreeMap)
	}
}

func TestNewPreservesExplicitCacheOptOut(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GPXDir = t.TempDir()
	cfg.ElevationTiles = false
	cfg.ElevationTileCache = ""
	cfg.OfflineCacheDir = ""
	cfg.OfflineMode = "cache-only"
	cfg.OpenFreeMapURL = ""
	cfg.StatsLogInterval = 0
	cfg.Assets = fstest.MapFS{"index.html": {Data: []byte("<head></head>")}}

	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/offline/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /offline/status status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response struct {
		Writable bool `json:"writable"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode /offline/status: %v", err)
	}
	if response.Writable {
		t.Error("offline cache is writable after explicit persistence opt-out")
	}
}

func TestNewRequiresBuiltFrontend(t *testing.T) {
	if _, ok := web.Assets(); ok {
		t.Skip("frontend is present in this build")
	}

	_, err := New(Config{GPXDir: t.TempDir()})
	if !errors.Is(err, ErrFrontendUnavailable) {
		t.Fatalf("New() error = %v, want %v", err, ErrFrontendUnavailable)
	}
}
