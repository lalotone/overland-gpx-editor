// Package server implements the GPX editor backend: a file library over a
// directory of .gpx files, a DEM elevation proxy, and the embedded frontend.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// Config wires a Server up. Only GPXDir is required.
type Config struct {
	// GPXDir is the track library directory. It is created if missing.
	GPXDir string
	// ElevationHost is a self-hosted opentopodata-style DEM service. Empty
	// uses tiles when enabled, otherwise the public Open-Meteo API.
	ElevationHost string
	// ElevationDataset is used for requests that do not name one. Applies to
	// ElevationHost only; Open-Meteo serves one dataset.
	ElevationDataset string
	// ElevationTiles reads elevation from terrain-RGB tiles instead of an
	// elevation API. Takes precedence over ElevationHost and Open-Meteo.
	ElevationTiles bool
	// ElevationTileURL overrides the tile template ({z}/{x}/{y}).
	ElevationTileURL string
	// ElevationTileZoom sets the tile zoom, and with it the resolution and
	// the bandwidth. 0 uses the default.
	ElevationTileZoom int
	// ElevationTileCache is a directory to keep fetched tiles in. Empty keeps
	// them in memory only, so nothing survives a restart.
	ElevationTileCache         string
	ElevationTileCacheMaxBytes int64
	// NominatimURL is the browser-facing place-search service. It is exposed
	// through /config so operators can switch providers without rebuilding.
	NominatimURL string
	// OfflineCacheDir stores privacy-sensitive provider responses. Empty
	// disables generic persistence while retaining bounded proxy behavior.
	OfflineCacheDir        string
	OfflineCacheMaxBytes   int64
	OfflineCacheMaxEntries int
	// OfflineMode is "auto" or "cache-only". Cache-only never uses an
	// outbound transport, including for elevation tile misses.
	OfflineMode string
	// UpstreamContact is appended to the stable outbound User-Agent.
	UpstreamContact string
	// TrustedUIOrigin authorizes cache-management requests from a separately
	// hosted frontend. It is never returned by /config.
	TrustedUIOrigin   string
	OfflineAdminToken string
	ValhallaURL       string
	OSRMURL           string
	OverpassURL       string
	FuelURL           string
	// OpenFreeMapURL enables the persistent OpenFreeMap-compatible map proxy.
	// The CLI supplies the public Liberty style by default.
	OpenFreeMapURL       string
	OpenFreeMapAllowBulk bool
	// HTTPClient is an injection seam for tests and controlled embeddings.
	HTTPClient *http.Client
	// StatsLogInterval controls privacy-safe aggregate operational logging.
	// Zero disables it; the CLI enables it once per minute by default.
	StatsLogInterval time.Duration
	// StatsLogger receives the aggregate operational table. Nil uses log.Default.
	StatsLogger *log.Logger
	// AllowedOrigins lists exact browser origins allowed to call the API.
	// Empty permits same-origin requests only when the request host is loopback.
	AllowedOrigins []string
	// MCPBrowserHandler serves the private MCP browser bridge. It lives on the
	// main listener because the page must reach it same-origin; the agent-facing
	// MCP endpoint is served from a separate loopback listener instead.
	// Nil keeps all MCP HTTP routes absent.
	MCPBrowserHandler http.Handler
	// BehindProxy withdraws implicit loopback trust. Behind a reverse proxy
	// every request arrives from the proxy, so a loopback peer address proves
	// nothing and management must be authorized explicitly.
	BehindProxy bool
	// Assets is the built frontend. When nil the server is API-only.
	Assets fs.FS
}

const (
	defaultNominatimURL            = "https://nominatim.openstreetmap.org"
	defaultValhallaURL             = "https://valhalla1.openstreetmap.de"
	defaultOSRMURL                 = "https://router.project-osrm.org"
	defaultOverpassURL             = "https://overpass-api.de/api/interpreter"
	defaultFuelURL                 = "https://energia.serviciosmin.gob.es/ServiciosRestCarburantes/PreciosCarburantes/EstacionesTerrestres/"
	defaultCacheBytes              = int64(1 << 30)
	defaultElevationTileCacheBytes = int64(1 << 30)
	defaultCacheEntries            = 100000
	maxInFlightRequests            = 32
	maxQueuedRequests              = 64
)

// Server is an http.Handler exposing the whole app.
type Server struct {
	gpxDir          string
	gpxRoot         *os.Root
	gpxMu           sync.RWMutex
	elevation       *elevationProxy
	nominatimURL    string
	modes           *offlineModeController
	cache           *cacheStore
	outbound        *outboundClient
	providers       map[string]*providerPolicy
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	trustedUIOrigin string
	adminToken      string
	openFreeMap     *openFreeMapManager
	rasterMaps      map[string]*rasterAdapter
	packs           *packManager
	allowedOrigins  map[string]struct{}
	mcpBrowser      http.Handler
	behindProxy     bool
	assets          fs.FS
	handler         http.Handler
	statsInterval   time.Duration
	statsLogger     *log.Logger
}

// New validates cfg, creates the GPX directory and returns the handler.
func New(cfg Config) (*Server, error) {
	if cfg.StatsLogInterval < 0 {
		return nil, errors.New("stats log interval cannot be negative")
	}
	allowedOrigins, err := normalizeOrigins(cfg.AllowedOrigins)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.GPXDir, 0o755); err != nil {
		return nil, err
	}
	gpxRoot, err := os.OpenRoot(cfg.GPXDir)
	if err != nil {
		return nil, err
	}
	mode := offlineMode(strings.TrimSpace(cfg.OfflineMode))
	if mode == "" {
		mode = modeAuto
	}
	if mode != modeAuto && mode != modeCacheOnly {
		gpxRoot.Close()
		return nil, errors.New("offline mode must be auto or cache-only")
	}
	if cfg.OfflineCacheMaxBytes == 0 {
		cfg.OfflineCacheMaxBytes = defaultCacheBytes
	}
	if cfg.OfflineCacheMaxEntries == 0 {
		cfg.OfflineCacheMaxEntries = defaultCacheEntries
	}
	if cfg.ElevationTileCacheMaxBytes == 0 {
		cfg.ElevationTileCacheMaxBytes = defaultElevationTileCacheBytes
	}
	if cfg.ElevationTileCacheMaxBytes < 0 {
		gpxRoot.Close()
		return nil, errors.New("elevation tile cache max bytes must be positive")
	}
	// Persisted pins are derived from pack manifests, so defer pin-aware
	// eviction until the manifests have been reconciled below.
	cache, err := openCacheStore(cfg.OfflineCacheDir, cfg.OfflineCacheMaxBytes, cfg.OfflineCacheMaxEntries, false)
	if err != nil {
		gpxRoot.Close()
		return nil, err
	}
	trustedUIOrigin := ""
	if strings.TrimSpace(cfg.TrustedUIOrigin) != "" {
		trustedUIOrigin, err = normalizeOrigin(cfg.TrustedUIOrigin)
		if err != nil {
			gpxRoot.Close()
			cache.close()
			return nil, fmt.Errorf("trusted UI origin: %w", err)
		}
		allowedOrigins[trustedUIOrigin] = struct{}{}
	}

	providerURLs := map[string]string{
		"nominatim": valueOrDefault(cfg.NominatimURL, defaultNominatimURL),
		"valhalla":  valueOrDefault(cfg.ValhallaURL, defaultValhallaURL),
		"osrm":      valueOrDefault(cfg.OSRMURL, defaultOSRMURL),
		"overpass":  valueOrDefault(cfg.OverpassURL, defaultOverpassURL),
		"fuel":      valueOrDefault(cfg.FuelURL, defaultFuelURL),
	}
	parsedURLs := make(map[string]*url.URL, len(providerURLs))
	for name, raw := range providerURLs {
		parsedURLs[name], err = parseProviderURL(name, raw)
		if err != nil {
			gpxRoot.Close()
			cache.close()
			return nil, err
		}
	}
	var openFreeMapURL *url.URL
	if strings.TrimSpace(cfg.OpenFreeMapURL) != "" {
		openFreeMapURL, err = parseProviderURL("openfreemap", cfg.OpenFreeMapURL)
		if err != nil {
			gpxRoot.Close()
			cache.close()
			return nil, err
		}
	}
	controlReserve := int64(maxStoredPackManifests * maxPackManifestBytes)
	if openFreeMapURL != nil {
		controlReserve += maxMapGenerationBytes
	}
	if err := cache.setReservedBytes(controlReserve); err != nil {
		gpxRoot.Close()
		cache.close()
		return nil, fmt.Errorf("reserve offline control storage: %w", err)
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	modes := newOfflineModeController(rootCtx, mode)
	// A DEM lookup of 100 points is not instant, but nothing about it should
	// take half a minute either.
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	} else {
		clone := *client
		client = &clone
		if client.Timeout == 0 {
			client.Timeout = 30 * time.Second
		}
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && (!strings.EqualFold(req.URL.Scheme, via[0].URL.Scheme) || !strings.EqualFold(req.URL.Host, via[0].URL.Host)) {
			return errors.New("redirect left approved provider origin")
		}
		return nil
	}
	contact := strings.TrimSpace(cfg.UpstreamContact)
	if contact == "" {
		contact = "https://github.com/lalotone/overland-gpx-editor"
	}
	ua := "gpx-editor/1 (" + strings.ReplaceAll(contact, ")", "") + ")"
	fossgis := newRateGroup(time.Second)
	nominatimGroup := newRateGroup(time.Second)
	overpassGroup := newRateGroup(time.Second)
	providers := map[string]*providerPolicy{
		"fuel":           newProviderPolicy("fuel", "fuel", parsedURLs["fuel"], 24*time.Hour, 30*24*time.Hour, 90*24*time.Hour, true, 32<<20, []string{"application/json"}, nil, true),
		"places":         newProviderPolicy("nominatim", "places", parsedURLs["nominatim"], 24*time.Hour, 30*24*time.Hour, 90*24*time.Hour, true, 2<<20, []string{"application/json"}, nominatimGroup, false),
		"pois":           newProviderPolicy("overpass", "pois", parsedURLs["overpass"], time.Hour, 7*24*time.Hour, 30*24*time.Hour, true, 8<<20, []string{"application/json"}, overpassGroup, true),
		"valhalla-route": newProviderPolicy("valhalla-route", "routing", parsedURLs["valhalla"], time.Hour, -1, 30*24*time.Hour, false, 16<<20, []string{"application/json"}, fossgis, false),
		"osrm-route":     newProviderPolicy("osrm-route", "routing", parsedURLs["osrm"], time.Hour, -1, 30*24*time.Hour, false, 16<<20, []string{"application/json"}, fossgis, false),
		"surface":        newProviderPolicy("valhalla-surface", "surface", parsedURLs["valhalla"], time.Hour, -1, 30*24*time.Hour, false, 16<<20, []string{"application/json"}, fossgis, false),
	}
	providers["valhalla-route"].fetchTimeout = routeOutboundFetchTimeout
	providers["osrm-route"].fetchTimeout = routeOutboundFetchTimeout
	providers["fuel"].maxFresh = 24 * time.Hour
	providers["fuel"].applicationData = true

	// An explicitly configured DEM host is a deliberate choice, so it wins over
	// the tile default.
	var tiles *tileStore
	if cfg.ElevationTiles && strings.TrimSpace(cfg.ElevationHost) == "" {
		tiles = newTileStoreWithQuota(cfg.ElevationTileURL, cfg.ElevationTileZoom, cfg.ElevationTileCache, cfg.ElevationTileCacheMaxBytes, client)
		if tiles.cacheErr != nil {
			cancel()
			gpxRoot.Close()
			cache.close()
			return nil, fmt.Errorf("open elevation tile cache: %w", tiles.cacheErr)
		}
	}

	nominatimURL := strings.TrimRight(parsedURLs["nominatim"].String(), "/")

	s := &Server{
		gpxDir:          cfg.GPXDir,
		gpxRoot:         gpxRoot,
		modes:           modes,
		cache:           cache,
		providers:       providers,
		ctx:             rootCtx,
		cancel:          cancel,
		trustedUIOrigin: trustedUIOrigin,
		adminToken:      cfg.OfflineAdminToken,
		elevation: &elevationProxy{
			tiles:          tiles,
			host:           strings.TrimSpace(cfg.ElevationHost),
			defaultDataset: cfg.ElevationDataset,
			client:         client,
		},
		nominatimURL:   nominatimURL,
		allowedOrigins: allowedOrigins,
		mcpBrowser:     cfg.MCPBrowserHandler,
		behindProxy:    cfg.BehindProxy,
		assets:         cfg.Assets,
		statsInterval:  cfg.StatsLogInterval,
		statsLogger:    cfg.StatsLogger,
	}
	if s.statsLogger == nil {
		s.statsLogger = log.Default()
	}
	// The tile store must join the server-owned lifecycle, not a detached
	// prefetch WaitGroup.
	if tiles != nil {
		tiles.configureLifecycle(modes, rootCtx, &s.wg)
		tiles.userAgent = ua
	}
	// Provider requests carry operation-specific context deadlines. Keep the
	// shared client's shorter ceiling for elevation tiles and direct DEM calls.
	outboundHTTPClient := *client
	outboundHTTPClient.Timeout = 0
	s.outbound = newOutboundClientWithModes(modes, cache, &outboundHTTPClient, rootCtx, &s.wg, ua)
	if client.Timeout > 0 && client.Timeout < s.outbound.fetchTimeout {
		s.outbound.fetchTimeout = client.Timeout
	}
	s.rasterMaps, err = newRasterAdapters()
	if err != nil {
		cancel()
		gpxRoot.Close()
		cache.close()
		if tiles != nil {
			tiles.closeCache()
		}
		return nil, err
	}
	s.elevation.outbound = s.outbound
	if s.elevation.tiles == nil {
		var elevationBase *url.URL
		if s.elevation.host != "" {
			elevationBase, err = parseProviderURL("elevation", s.elevation.host)
		} else {
			elevationBase, err = parseProviderURL("open-meteo", openMeteoURL)
		}
		if err != nil {
			cancel()
			gpxRoot.Close()
			cache.close()
			if tiles != nil {
				tiles.closeCache()
			}
			return nil, err
		}
		s.elevation.policy = newProviderPolicy("elevation", "elevation", elevationBase, 24*time.Hour, 30*24*time.Hour, 90*24*time.Hour, true, maxElevationBodyBytes, []string{"application/json", "text/plain"}, newConcurrentRateGroup(0, 2), true)
	}
	if openFreeMapURL != nil {
		s.openFreeMap = newOpenFreeMapManager(s, openFreeMapURL, cfg.OpenFreeMapAllowBulk)
	}
	s.packs, err = newPackManager(s)
	if err != nil {
		cancel()
		gpxRoot.Close()
		cache.close()
		if tiles != nil {
			tiles.closeCache()
		}
		return nil, err
	}
	if err := cache.enforceLoadedLimits(); err != nil {
		cancel()
		gpxRoot.Close()
		cache.close()
		if tiles != nil {
			tiles.closeCache()
		}
		return nil, fmt.Errorf("enforce offline cache limits: %w", err)
	}
	s.handler = s.routes()
	if s.openFreeMap != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.openFreeMap.activate(rootCtx)
		}()
	}
	if s.statsInterval > 0 {
		s.wg.Add(1)
		go s.logOperationalStatsLoop()
	}
	return s, nil
}

// Close releases the directory handle used to confine library operations.
// Call it after the HTTP server has stopped accepting requests.
func (s *Server) Close() error {
	s.cancel()
	if s.elevation.tiles != nil {
		s.elevation.tiles.close()
	}
	s.wg.Wait()
	s.cache.flushAccesses()
	var tileCacheErr error
	if s.elevation.tiles != nil {
		tileCacheErr = s.elevation.tiles.closeCache()
	}
	s.gpxMu.Lock()
	gpxErr := s.gpxRoot.Close()
	s.gpxMu.Unlock()
	return errors.Join(gpxErr, s.cache.close(), tileCacheErr)
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func newProviderPolicy(name, scope string, base *url.URL, fresh, stale, retention time.Duration, staleOnError bool, maxBody int64, contentTypes []string, group *rateGroup, packEligible bool) *providerPolicy {
	allowStale := stale >= 0
	if stale < 0 {
		stale = 0
	}
	return &providerPolicy{name: name, scope: scope, baseURL: base, sourceFingerprint: sourceFingerprint(base), fallbackFresh: fresh, maxStale: stale, allowStale: allowStale, retention: retention, staleOnError: staleOnError, maxBody: maxBody, contentTypes: contentTypes, group: group, packEligible: packEligible, approvedHosts: map[string]struct{}{strings.ToLower(base.Host): {}}}
}

// mcpBrowserEventsPath is the long-lived MCP browser command stream. It is
// held open for the lifetime of a tab, so it must not occupy a slot in the
// request throttle shared with the rest of the API.
const mcpBrowserEventsPath = "/mcp/browser/events"

// skipPath applies middleware to every request except one exact path.
func skipPath(path string, mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == path {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Use(skipPath(mcpBrowserEventsPath, middleware.ThrottleBacklog(maxInFlightRequests, maxQueuedRequests, 5*time.Second)))
	r.Use(middleware.Compress(5))
	r.Use(middleware.GetHead)
	r.Use(s.protectBrowserWrites)
	if len(s.allowedOrigins) > 0 {
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins:     mapKeys(s.allowedOrigins),
			AllowedMethods:     []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
			AllowedHeaders:     []string{"Accept", "Content-Type", "Authorization", "X-GPX-Editor"},
			ExposedHeaders:     []string{"X-GPX-Cache", "X-GPX-Cached-At", "Age"},
			AllowCredentials:   false,
			MaxAge:             300,
			OptionsPassthrough: true,
		}))
	}

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	r.Get("/files", s.handleListFiles)
	r.Get("/gpx/{filename}", s.handleGetFile)
	r.Put("/gpx/{filename}", s.handleSaveFile)
	r.Post("/gpx/{filename}", s.handleSaveFile)
	r.Delete("/gpx/{filename}", s.handleDeleteFile)
	r.Post("/upload", s.handleUpload)
	r.Get("/elevation", s.protectOutboundResource(s.handleElevation))
	r.Post("/elevation/batch", s.protectOutboundResource(s.handleElevationBatch))
	r.Post("/elevation/prefetch", s.protectOutboundResource(s.handlePrefetch))
	r.Get("/elevation/prefetch", s.handlePrefetchStatus)
	r.Get("/fuel", s.protectOutboundResource(s.handleFuel))
	r.Get("/places/search", s.protectOutboundResource(s.handlePlaceSearch))
	r.Post("/pois/search", s.protectOutboundResource(s.handlePOISearch))
	r.Post("/routing/valhalla/route", s.protectOutboundResource(s.handleValhallaRoute))
	r.Post("/routing/osrm/route", s.protectOutboundResource(s.handleOSRMRoute))
	r.Post("/routing/valhalla/surface", s.protectOutboundResource(s.handleValhallaSurface))
	r.Get("/map/raster/{layer}/{z}/{x}/{y}", s.protectOutboundResource(s.handleRasterMap))
	r.Get("/map/openfreemap/style.json", s.protectOutboundResource(func(w http.ResponseWriter, r *http.Request) {
		if s.openFreeMap == nil {
			writeError(w, http.StatusNotFound, "OpenFreeMap-compatible source is not configured")
			return
		}
		s.openFreeMap.handleStyle(w, r)
	}))
	r.Get("/map/openfreemap/source/{source}", s.protectOutboundResource(func(w http.ResponseWriter, r *http.Request) {
		if s.openFreeMap == nil {
			writeError(w, http.StatusNotFound, "OpenFreeMap-compatible source is not configured")
			return
		}
		s.openFreeMap.handleSource(w, r)
	}))
	r.Get("/map/openfreemap/source.json", s.protectOutboundResource(func(w http.ResponseWriter, r *http.Request) {
		if s.openFreeMap == nil {
			writeError(w, http.StatusNotFound, "OpenFreeMap-compatible source is not configured")
			return
		}
		s.openFreeMap.handleSource(w, r)
	}))
	r.Get("/map/openfreemap/tiles/{source}/{z}/{x}/{y}", s.protectOutboundResource(func(w http.ResponseWriter, r *http.Request) {
		if s.openFreeMap == nil {
			writeError(w, http.StatusNotFound, "OpenFreeMap-compatible source is not configured")
			return
		}
		s.openFreeMap.handleTile(w, r, false)
	}))
	r.Get("/map/openfreemap/tiles/{z}/{x}/{y}", s.protectOutboundResource(func(w http.ResponseWriter, r *http.Request) {
		if s.openFreeMap == nil {
			writeError(w, http.StatusNotFound, "OpenFreeMap-compatible source is not configured")
			return
		}
		s.openFreeMap.handleTile(w, r, false)
	}))
	r.Get("/map/openfreemap/raster/{source}/{z}/{x}/{y}", s.protectOutboundResource(func(w http.ResponseWriter, r *http.Request) {
		if s.openFreeMap == nil {
			writeError(w, http.StatusNotFound, "OpenFreeMap-compatible source is not configured")
			return
		}
		s.openFreeMap.handleTile(w, r, true)
	}))
	r.Get("/map/openfreemap/glyphs/{fontstack}/{range}", s.protectOutboundResource(func(w http.ResponseWriter, r *http.Request) {
		if s.openFreeMap == nil {
			writeError(w, http.StatusNotFound, "OpenFreeMap-compatible source is not configured")
			return
		}
		s.openFreeMap.handleGlyph(w, r)
	}))
	r.Get("/map/openfreemap/{variant:sprite(?:@2x)?\\.(?:json|png)}", s.protectOutboundResource(func(w http.ResponseWriter, r *http.Request) {
		if s.openFreeMap == nil {
			writeError(w, http.StatusNotFound, "OpenFreeMap-compatible source is not configured")
			return
		}
		s.openFreeMap.handleSprite(w, r)
	}))
	r.Get("/offline/status", s.handleOfflineStatus)
	r.Put("/offline/mode", s.requireOfflineControl(s.handleOfflineMode))
	r.Get("/offline/packs", s.requireOfflineRead(s.handleListPacks))
	r.Post("/offline/packs/estimate", s.requireOfflineControl(s.handleEstimatePack))
	r.Post("/offline/packs", s.requireOfflineControl(s.handleCreatePack))
	r.Get("/offline/packs/{id}", s.requireOfflineRead(s.handleGetPack))
	r.Post("/offline/packs/{id}/cancel", s.requireOfflineControl(s.handleCancelPack))
	r.Delete("/offline/packs/{id}", s.requireOfflineControl(s.handleDeletePack))
	r.Delete("/offline/cache", s.requireOfflineControl(s.handleClearCache))
	r.Options("/offline/*", s.handleOfflineOptions)
	r.Get("/config", s.handleConfig)
	if s.mcpBrowser != nil {
		r.Mount("/mcp/browser", http.StripPrefix("/mcp/browser", s.mcpBrowser))
	}
	r.Options("/*", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	r.Get("/*", s.assetHandler().ServeHTTP)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "Not found")
	})
	return r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func normalizeOrigins(origins []string) (map[string]struct{}, error) {
	normalized := make(map[string]struct{}, len(origins))
	for _, raw := range origins {
		origin, err := normalizeOrigin(raw)
		if err != nil {
			return nil, fmt.Errorf("allowed origin %q: %w", raw, err)
		}
		normalized[origin] = struct{}{}
	}
	return normalized, nil
}

func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must be an exact http or https origin")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	return keys
}

func (s *Server) protectBrowserWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut &&
			r.Method != http.MethodPatch && r.Method != http.MethodDelete {
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")
		if origin == "" {
			if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
				writeError(w, http.StatusForbidden, "Cross-origin request refused")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		normalized, err := normalizeOrigin(origin)
		if err == nil {
			if strings.HasPrefix(r.URL.Path, "/offline/") && s.isTrustedManagementOrigin(origin, r) {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := s.allowedOrigins[normalized]; ok {
				next.ServeHTTP(w, r)
				return
			}
			if len(s.allowedOrigins) == 0 && loopbackSameOrigin(normalized, r.Host) {
				next.ServeHTTP(w, r)
				return
			}
		}

		writeError(w, http.StatusForbidden, "Cross-origin request refused")
	})
}

func loopbackSameOrigin(origin, requestHost string) bool {
	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Host, requestHost) {
		return false
	}
	return loopbackOrigin(origin)
}

func loopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

/* -- Frontend --------------------------------------------------------- */

func (s *Server) assetHandler() http.Handler {
	if s.assets == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound,
				"No frontend embedded in this binary — run `npm run build` and rebuild")
		})
	}

	files := http.FileServerFS(s.assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		// Optional API routes must never masquerade as a successful session
		// by falling through to the SPA shell.
		if name == "mcp" || strings.HasPrefix(name, "mcp/") {
			http.NotFound(w, r)
			return
		}
		if name == "" {
			name = "index.html"
		}
		if name == "index.html" {
			s.serveIndex(w, r)
			return
		}

		if _, err := fs.Stat(s.assets, name); err != nil {
			// Unknown path: hand it to the SPA router rather than 404ing, so
			// deep links keep working. Asset-looking paths still 404.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			s.serveIndex(w, r)
			return
		}

		// Vite fingerprints everything under /assets, so those are immutable.
		// index.html must not be cached or a rebuild is invisible to browsers.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	index, err := fs.ReadFile(s.assets, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	marker := []byte(`<meta name="gpx-editor-offline-mode" content="` + string(s.modes.mode()) + `">`)
	if bytes.Contains(index, []byte("<head>")) {
		index = bytes.Replace(index, []byte("<head>"), append([]byte("<head>"), marker...), 1)
	} else {
		index = append(index, marker...)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(index)
}

/* -- Responses -------------------------------------------------------- */

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

// writeError mirrors the FastAPI error shape the frontend already knows.
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
