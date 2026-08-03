// Package server implements the GPX editor backend: a file library over a
// directory of .gpx files, a DEM elevation proxy, and the embedded frontend.
package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// Config wires a Server up. Only GPXDir is required.
type Config struct {
	// GPXDir is the track library directory. It is created if missing.
	GPXDir string
	// ElevationHost is a self-hosted opentopodata-style DEM service. Empty
	// uses the public Open-Meteo API, which needs no setup.
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
	// Assets is the built frontend. When nil the server is API-only.
	Assets fs.FS
}

// Server is an http.Handler exposing the whole app.
type Server struct {
	gpxDir    string
	elevation *elevationProxy
	assets    fs.FS
	mux       *http.ServeMux
}

// New validates cfg, creates the GPX directory and returns the handler.
func New(cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.GPXDir, 0o755); err != nil {
		return nil, err
	}
	// A DEM lookup of 100 points is not instant, but nothing about it should
	// take half a minute either.
	client := &http.Client{Timeout: 30 * time.Second}

	var tiles *tileStore
	if cfg.ElevationTiles {
		tiles = newTileStore(cfg.ElevationTileURL, cfg.ElevationTileZoom, cfg.ElevationTileCache, client)
	}

	s := &Server{
		gpxDir: cfg.GPXDir,
		elevation: &elevationProxy{
			tiles:          tiles,
			host:           strings.TrimSpace(cfg.ElevationHost),
			defaultDataset: cfg.ElevationDataset,
			client:         client,
		},
		assets: cfg.Assets,
		mux:    http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /files", s.handleListFiles)
	s.mux.HandleFunc("GET /gpx/{filename}", s.handleGetFile)
	s.mux.HandleFunc("PUT /gpx/{filename}", s.handleSaveFile)
	s.mux.HandleFunc("POST /gpx/{filename}", s.handleSaveFile)
	s.mux.HandleFunc("DELETE /gpx/{filename}", s.handleDeleteFile)
	s.mux.HandleFunc("POST /upload", s.handleUpload)
	s.mux.HandleFunc("GET /elevation", s.handleElevation)
	s.mux.HandleFunc("POST /elevation/batch", s.handleElevationBatch)
	s.mux.HandleFunc("POST /elevation/prefetch", s.handlePrefetch)
	s.mux.HandleFunc("GET /elevation/prefetch", s.handlePrefetchStatus)
	s.mux.Handle("/", s.assetHandler())
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS is handled ahead of the mux so preflight requests to routes that
	// only accept PUT/DELETE do not fall through to a 405.
	//
	// allow_credentials stays off while the origin is "*" — browsers reject
	// the combination, and this API has no cookie or auth story that needs it.
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Vary", "Origin")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mux.ServeHTTP(w, r)
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
