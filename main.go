// Command gpx-editor serves the GPX editor — frontend and API — from a single
// self-contained binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lalotone/overland-gpx-editor/internal/server"
	"github.com/lalotone/overland-gpx-editor/web"
)

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8000"),
		"address to listen on")
	gpxDir := flag.String("gpx-dir", envOr("GPX_DIR", "gpx"),
		"directory holding the track library")
	elevationHost := flag.String("elevation-host", envOr("ELEVATION_HOST", ""),
		"self-hosted opentopodata-style DEM service; empty uses the public Open-Meteo API")
	elevationDataset := flag.String("elevation-dataset", envOr("ELEVATION_DATASET", "srtm30m"),
		"DEM dataset for -elevation-host when a request does not name one")
	flag.Parse()

	assets, hasUI := web.Assets()
	if !hasUI {
		log.Print("no frontend embedded — run `npm run build` and rebuild to serve the UI")
	}

	srv, err := server.New(server.Config{
		GPXDir:           *gpxDir,
		ElevationHost:    *elevationHost,
		ElevationDataset: *elevationDataset,
		Assets:           assets,
	})
	if err != nil {
		log.Fatalf("gpx-editor: %v", err)
	}

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: logRequests(srv),
		// Uploads and DEM proxying can be slow; only the header deadline is
		// safe to keep tight.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	elevationSource := "Open-Meteo (public, Copernicus 90 m)"
	if *elevationHost != "" {
		elevationSource = fmt.Sprintf("%s (%s)", *elevationHost, *elevationDataset)
	}

	// The API reads, writes and deletes files in the library directory and has
	// no authentication. Bind it to a trusted network only.
	log.Printf("gpx-editor listening on %s (library: %s, elevation: %s)",
		*addr, *gpxDir, elevationSource)

	shutdown := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		close(shutdown)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("gpx-editor: %v", err)
	}
	<-shutdown
	log.Print("gpx-editor stopped")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Static assets are noisy and uninteresting once they work.
		if rec.status >= 400 || !isAsset(r.URL.Path) {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status,
				time.Since(start).Round(time.Millisecond))
		}
	})
}

func isAsset(path string) bool {
	return strings.HasPrefix(path, "/assets/")
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
