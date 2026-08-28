// Package server implements the GPX editor backend: a file library over a
// directory of .gpx files, a DEM elevation proxy, and the embedded frontend.
package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
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
	ElevationTileCache string
	// NominatimURL is the browser-facing place-search service. It is exposed
	// through /config so operators can switch providers without rebuilding.
	NominatimURL string
	// AllowedOrigins lists exact browser origins allowed to call the API.
	// Empty permits same-origin requests only when the request host is loopback.
	AllowedOrigins []string
	// Assets is the built frontend. When nil the server is API-only.
	Assets fs.FS
}

const (
	defaultNominatimURL = "https://nominatim.openstreetmap.org"
	maxInFlightRequests = 32
	maxQueuedRequests   = 64
)

// Server is an http.Handler exposing the whole app.
type Server struct {
	gpxDir         string
	gpxRoot        *os.Root
	gpxMu          sync.RWMutex
	elevation      *elevationProxy
	nominatimURL   string
	allowedOrigins map[string]struct{}
	assets         fs.FS
	handler        http.Handler
}

// New validates cfg, creates the GPX directory and returns the handler.
func New(cfg Config) (*Server, error) {
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
	// A DEM lookup of 100 points is not instant, but nothing about it should
	// take half a minute either.
	client := &http.Client{Timeout: 30 * time.Second}

	// An explicitly configured DEM host is a deliberate choice, so it wins over
	// the tile default.
	var tiles *tileStore
	if cfg.ElevationTiles && strings.TrimSpace(cfg.ElevationHost) == "" {
		tiles = newTileStore(cfg.ElevationTileURL, cfg.ElevationTileZoom, cfg.ElevationTileCache, client)
	}

	nominatimURL := strings.TrimRight(strings.TrimSpace(cfg.NominatimURL), "/")
	if nominatimURL == "" {
		nominatimURL = defaultNominatimURL
	}

	s := &Server{
		gpxDir:  cfg.GPXDir,
		gpxRoot: gpxRoot,
		elevation: &elevationProxy{
			tiles:          tiles,
			host:           strings.TrimSpace(cfg.ElevationHost),
			defaultDataset: cfg.ElevationDataset,
			client:         client,
		},
		nominatimURL:   nominatimURL,
		allowedOrigins: allowedOrigins,
		assets:         cfg.Assets,
	}
	s.handler = s.routes()
	return s, nil
}

// Close releases the directory handle used to confine library operations.
// Call it after the HTTP server has stopped accepting requests.
func (s *Server) Close() error {
	s.gpxMu.Lock()
	defer s.gpxMu.Unlock()
	return s.gpxRoot.Close()
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Use(middleware.ThrottleBacklog(maxInFlightRequests, maxQueuedRequests, 5*time.Second))
	r.Use(middleware.Compress(5))
	r.Use(middleware.GetHead)
	r.Use(s.protectBrowserWrites)
	if len(s.allowedOrigins) > 0 {
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins:     mapKeys(s.allowedOrigins),
			AllowedMethods:     []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
			AllowedHeaders:     []string{"Accept", "Content-Type"},
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
	r.Get("/elevation", s.handleElevation)
	r.Post("/elevation/batch", s.handleElevationBatch)
	r.Post("/elevation/prefetch", s.handlePrefetch)
	r.Get("/elevation/prefetch", s.handlePrefetchStatus)
	r.Get("/config", s.handleConfig)
	r.Options("/*", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	r.Get("/*", s.assetHandler().ServeHTTP)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "Not found")
	})
	return r
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, struct {
		NominatimURL string `json:"nominatimUrl"`
	}{NominatimURL: s.nominatimURL})
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
		if name == "" {
			name = "index.html"
		}

		if _, err := fs.Stat(s.assets, name); err != nil {
			// Unknown path: hand it to the SPA router rather than 404ing, so
			// deep links keep working. Asset-looking paths still 404.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r, s.assets)
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

func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
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
