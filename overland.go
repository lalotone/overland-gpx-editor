// Package overland exposes the complete Overland application as an embeddable
// HTTP handler. The host owns the listener, authentication and HTTP shutdown.
package overland

import (
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lalotone/overland-gpx-editor/internal/server"
	"github.com/lalotone/overland-gpx-editor/web"
)

// ErrFrontendUnavailable means the React frontend was not built before the Go
// package was compiled. Run npm run build in the Overland module first.
var ErrFrontendUnavailable = errors.New("overland: embedded frontend is unavailable; run npm run build before building the host application")

// Config configures an embedded Overland application. Start with DefaultConfig
// for CLI defaults, then override settings explicitly. New does not merge it
// with DefaultConfig or read CLI environment variables.
type Config struct {
	// GPXDir is the required track library directory. It is created if missing.
	GPXDir string
	// ElevationHost selects a self-hosted opentopodata-style DEM, taking
	// precedence over tiles. Empty uses tiles if enabled, otherwise Open-Meteo.
	ElevationHost string
	// ElevationDataset is the default dataset used with ElevationHost.
	ElevationDataset string
	// ElevationTiles enables terrain-RGB elevation tiles.
	ElevationTiles bool
	// ElevationTileURL overrides the tile URL template ({z}/{x}/{y}).
	ElevationTileURL string
	// ElevationTileZoom sets the tile zoom. Zero uses 13.
	ElevationTileZoom int
	// ElevationTileCache persists fetched terrain tiles. Empty is memory-only.
	ElevationTileCache string
	// ElevationTileCacheMaxBytes bounds the terrain cache. Zero uses 1 GiB.
	ElevationTileCacheMaxBytes int64

	// OfflineCacheDir persists provider responses and trip packs. Empty disables
	// persistence, not the proxy or offline policy. Cached data may be sensitive.
	OfflineCacheDir string
	// OfflineCacheMaxBytes and OfflineCacheMaxEntries bound the response cache,
	// including metadata. Zero uses 1 GiB and 100000 entries respectively.
	OfflineCacheMaxBytes   int64
	OfflineCacheMaxEntries int
	// OfflineMode is "auto" (also the zero value) or "cache-only". Starting in
	// cache-only forbids all upstream requests and locks out runtime online mode.
	OfflineMode string

	// Provider URLs are startup-only overrides. Empty uses the public services.
	// NominatimURL is also exposed to the frontend through /config.
	NominatimURL string
	ValhallaURL  string
	OSRMURL      string
	OverpassURL  string
	FuelURL      string
	// OpenFreeMapURL is an OpenFreeMap-compatible style URL. Unlike the other
	// provider URLs, empty disables this proxy. DefaultConfig enables Liberty.
	OpenFreeMapURL string
	// OpenFreeMapAllowBulk permits bounded trip-pack fetching from that source.
	OpenFreeMapAllowBulk bool
	// UpstreamContact is included in the outbound User-Agent. Empty uses the
	// project URL; deployments should supply their operator contact.
	UpstreamContact string
	// HTTPClient optionally supplies the upstream transport. The client is
	// copied, with bounded timeouts and same-origin redirect checks enforced.
	// The host retains ownership of its transport.
	HTTPClient *http.Client

	// AllowedOrigins lists exact browser origins allowed to call the API.
	// Empty permits browser writes only from matching loopback origins.
	// Origin checks are not authentication; protect non-loopback deployments.
	AllowedOrigins []string
	// BehindProxy withdraws implicit loopback peer trust. Set it whenever a
	// reverse proxy sits in front of this handler.
	BehindProxy bool
	// TrustedUIOrigin is an exact browser origin authorized for offline
	// management. It also permits that origin to call the rest of the API.
	TrustedUIOrigin string
	// OfflineAdminToken authorizes offline management with a Bearer token.
	// It is not authentication for the track library or the rest of the API.
	OfflineAdminToken string

	// StatsLogInterval enables aggregate operational logs. Zero disables them.
	StatsLogInterval time.Duration
	// StatsLogger receives aggregate logs. Nil uses log.Default().
	StatsLogger *log.Logger
	// Assets overrides the bundled frontend, primarily for tests or a custom UI.
	// It must be rooted at index.html and support http.FileServerFS. Nil uses
	// Overland's frontend embedded at build time.
	Assets fs.FS
}

// DefaultConfig returns the same backend defaults as the Overland CLI, without
// applying CLI environment overrides. Only XDG paths and the user's home are
// consulted. No directories are created until New is called.
func DefaultConfig() Config {
	return Config{
		GPXDir:                     DefaultGPXDir(),
		ElevationDataset:           "srtm30m",
		ElevationTiles:             true,
		ElevationTileCache:         DefaultTileCacheDir(),
		ElevationTileCacheMaxBytes: 1 << 30,
		OfflineCacheDir:            DefaultOfflineCacheDir(),
		OfflineCacheMaxBytes:       1 << 30,
		OfflineCacheMaxEntries:     100000,
		OfflineMode:                "auto",
		NominatimURL:               "https://nominatim.openstreetmap.org",
		ValhallaURL:                "https://valhalla1.openstreetmap.de",
		OSRMURL:                    "https://router.project-osrm.org",
		OverpassURL:                "https://overpass-api.de/api/interpreter",
		FuelURL:                    "https://energia.serviciosmin.gob.es/ServiciosRestCarburantes/PreciosCarburantes/EstacionesTerrestres/",
		OpenFreeMapURL:             "https://tiles.openfreemap.org/styles/liberty",
		OpenFreeMapAllowBulk:       true,
		UpstreamContact:            "https://github.com/lalotone/overland-gpx-editor",
		StatsLogInterval:           time.Minute,
	}
}

// DefaultGPXDir returns Overland's default track library directory.
func DefaultGPXDir() string {
	return dataPath("XDG_DATA_HOME", filepath.Join(".local", "share"), "gpx")
}

// DefaultTileCacheDir returns Overland's default terrain tile cache directory.
func DefaultTileCacheDir() string {
	return dataPath("XDG_CACHE_HOME", ".cache", "tiles")
}

// DefaultOfflineCacheDir returns Overland's default provider response cache directory.
func DefaultOfflineCacheDir() string {
	return dataPath("XDG_CACHE_HOME", ".cache", "responses")
}

func dataPath(envKey, homeFallback, leaf string) string {
	if base := os.Getenv(envKey); filepath.IsAbs(base) {
		return filepath.Join(base, "overland", leaf)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return leaf
	}
	return filepath.Join(home, homeFallback, "overland", leaf)
}

// App is a complete Overland frontend and API. It implements http.Handler.
// It does not start a listener or enable MCP.
type App struct {
	server *server.Server
}

// New builds an embeddable Overland application. Close must be called after the
// host stops serving requests. In auto mode the configured OpenFreeMap source
// begins loading asynchronously; provider failures are reported through the API.
func New(cfg Config) (*App, error) {
	assets := cfg.Assets
	if assets == nil {
		var ok bool
		assets, ok = web.Assets()
		if !ok {
			return nil, ErrFrontendUnavailable
		}
	}

	srv, err := server.New(server.Config{
		GPXDir:                     cfg.GPXDir,
		ElevationHost:              cfg.ElevationHost,
		ElevationDataset:           cfg.ElevationDataset,
		ElevationTiles:             cfg.ElevationTiles,
		ElevationTileURL:           cfg.ElevationTileURL,
		ElevationTileZoom:          cfg.ElevationTileZoom,
		ElevationTileCache:         cfg.ElevationTileCache,
		ElevationTileCacheMaxBytes: cfg.ElevationTileCacheMaxBytes,
		OfflineCacheDir:            cfg.OfflineCacheDir,
		OfflineCacheMaxBytes:       cfg.OfflineCacheMaxBytes,
		OfflineCacheMaxEntries:     cfg.OfflineCacheMaxEntries,
		OfflineMode:                cfg.OfflineMode,
		NominatimURL:               cfg.NominatimURL,
		ValhallaURL:                cfg.ValhallaURL,
		OSRMURL:                    cfg.OSRMURL,
		OverpassURL:                cfg.OverpassURL,
		FuelURL:                    cfg.FuelURL,
		OpenFreeMapURL:             cfg.OpenFreeMapURL,
		OpenFreeMapAllowBulk:       cfg.OpenFreeMapAllowBulk,
		UpstreamContact:            cfg.UpstreamContact,
		HTTPClient:                 cfg.HTTPClient,
		AllowedOrigins:             cfg.AllowedOrigins,
		BehindProxy:                cfg.BehindProxy,
		TrustedUIOrigin:            cfg.TrustedUIOrigin,
		OfflineAdminToken:          cfg.OfflineAdminToken,
		StatsLogInterval:           cfg.StatsLogInterval,
		StatsLogger:                cfg.StatsLogger,
		Assets:                     assets,
	})
	if err != nil {
		return nil, err
	}
	return &App{server: srv}, nil
}

// ServeHTTP serves Overland's frontend and API.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.server.ServeHTTP(w, r)
}

// Close cancels background work, waits for it to finish, and releases the
// application's directory handles. Call it once, after HTTP shutdown completes.
// It does not close a caller-supplied HTTP transport or Assets filesystem.
func (a *App) Close() error {
	return a.server.Close()
}
