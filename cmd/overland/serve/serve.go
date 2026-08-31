package serve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/lalotone/overland-gpx-editor/internal/server"
	"github.com/lalotone/overland-gpx-editor/web"
	"github.com/urfave/cli/v3"
)

const defaultOpenFreeMapURL = "https://tiles.openfreemap.org/styles/liberty"

var Command = &cli.Command{
	Name:   "serve",
	Usage:  "Serve the GPX editor and track library",
	Flags:  Flags(),
	Action: Run,
}

// Flags returns a fresh serve flag set.
func Flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "addr",
			Usage:   "address to listen on",
			Value:   "127.0.0.1:8000",
			Sources: util.NonEmptyEnv("ADDR"),
		},
		util.GPXDirFlag(),
		&cli.StringFlag{
			Name:    "elevation-host",
			Usage:   "self-hosted opentopodata-style DEM service; empty uses tiles or Open-Meteo",
			Sources: util.NonEmptyEnv("ELEVATION_HOST"),
		},
		&cli.StringFlag{
			Name:    "elevation-dataset",
			Usage:   "DEM dataset for --elevation-host when a request does not name one",
			Value:   "srtm30m",
			Sources: util.NonEmptyEnv("ELEVATION_DATASET"),
		},
		&cli.BoolFlag{
			Name:    "elevation-tiles",
			Usage:   "read elevation from terrain-RGB tiles (~30 m)",
			Value:   true,
			Sources: util.BoolEnv("ELEVATION_TILES"),
		},
		&cli.IntFlag{
			Name:    "elevation-tile-zoom",
			Usage:   "tile zoom: higher is finer and heavier (0 uses 13, ~14 m/px)",
			Sources: util.IntEnv("ELEVATION_TILE_ZOOM"),
		},
		&cli.StringFlag{
			Name:    "elevation-tile-cache",
			Usage:   "directory to keep fetched terrain tiles in",
			Value:   util.DefaultTileCacheDir(),
			Sources: util.NonEmptyEnv("ELEVATION_TILE_CACHE"),
		},
		&cli.StringFlag{
			Name:    "elevation-tile-cache-max-bytes",
			Usage:   "terrain tile cache byte quota (for example 1GiB)",
			Value:   "1GiB",
			Sources: util.NonEmptyEnv("ELEVATION_TILE_CACHE_MAX_BYTES"),
		},
		&cli.StringFlag{
			Name:    "nominatim-url",
			Usage:   "Nominatim-compatible place-search provider",
			Value:   "https://nominatim.openstreetmap.org",
			Sources: util.NonEmptyEnv("NOMINATIM_URL"),
		},
		&cli.StringFlag{Name: "offline-cache-dir", Usage: "persistent provider response cache; empty disables persistence", Value: util.DefaultOfflineCacheDir(), Sources: util.StringEnv("OFFLINE_CACHE_DIR")},
		&cli.StringFlag{Name: "offline-cache-max-bytes", Usage: "response-cache byte quota (for example 1GiB)", Value: "1GiB", Sources: util.NonEmptyEnv("OFFLINE_CACHE_MAX_BYTES")},
		&cli.IntFlag{Name: "offline-cache-max-entries", Usage: "response-cache entry limit", Value: 100000, Sources: util.IntEnv("OFFLINE_CACHE_MAX_ENTRIES")},
		&cli.StringFlag{Name: "offline-mode", Usage: "outbound mode: auto or cache-only", Value: "auto", Sources: util.NonEmptyEnv("OFFLINE_MODE")},
		&cli.StringFlag{Name: "upstream-contact", Usage: "operator contact included in outbound User-Agent", Value: "https://github.com/lalotone/overland-gpx-editor", Sources: util.NonEmptyEnv("UPSTREAM_CONTACT")},
		&cli.StringFlag{Name: "trusted-ui-origin", Usage: "exact remote UI origin allowed to manage offline data", Sources: util.NonEmptyEnv("TRUSTED_UI_ORIGIN")},
		&cli.StringFlag{Name: "offline-admin-token", Usage: "Bearer token for non-loopback offline management", Sources: util.StringEnv("OFFLINE_ADMIN_TOKEN")},
		&cli.StringFlag{Name: "valhalla-url", Usage: "Valhalla provider base URL", Value: "https://valhalla1.openstreetmap.de", Sources: util.NonEmptyEnv("VALHALLA_URL")},
		&cli.StringFlag{Name: "osrm-url", Usage: "OSRM provider base URL", Value: "https://router.project-osrm.org", Sources: util.NonEmptyEnv("OSRM_URL")},
		&cli.StringFlag{Name: "overpass-url", Usage: "Overpass interpreter URL", Value: "https://overpass-api.de/api/interpreter", Sources: util.NonEmptyEnv("OVERPASS_URL")},
		&cli.StringFlag{Name: "fuel-url", Usage: "Spanish fuel snapshot URL", Value: "https://energia.serviciosmin.gob.es/ServiciosRestCarburantes/PreciosCarburantes/EstacionesTerrestres/", Sources: util.NonEmptyEnv("FUEL_URL")},
		&cli.StringFlag{Name: "openfreemap-url", Usage: "OpenFreeMap-compatible style source", Value: defaultOpenFreeMapURL, Sources: util.NonEmptyEnv("OPENFREEMAP_URL")},
		&cli.BoolFlag{Name: "openfreemap-allow-bulk", Usage: "allow bounded trip-pack fetches from the configured map source", Value: true, Sources: util.BoolEnv("OPENFREEMAP_ALLOW_BULK")},
		&cli.StringSliceFlag{
			Name:    "allowed-origin",
			Usage:   "exact browser origin allowed to call the API; repeat for multiple origins",
			Sources: util.NonEmptyEnv("ALLOWED_ORIGINS"),
		},
	}
}

func Run(ctx context.Context, cmd *cli.Command) error {
	assets, hasUI := web.Assets()
	if !hasUI {
		log.Print("no frontend embedded - run `npm run build` and rebuild to serve the UI")
	}

	elevationHost := cmd.String("elevation-host")
	elevationTiles := cmd.Bool("elevation-tiles")
	tileZoom := cmd.Int("elevation-tile-zoom")
	tileCache := cmd.String("elevation-tile-cache")
	cacheBytes, err := parseByteSize(cmd.String("offline-cache-max-bytes"))
	if err != nil {
		return fmt.Errorf("offline-cache-max-bytes: %w", err)
	}
	tileCacheBytes, err := parseByteSize(cmd.String("elevation-tile-cache-max-bytes"))
	if err != nil {
		return fmt.Errorf("elevation-tile-cache-max-bytes: %w", err)
	}
	srv, err := server.New(server.Config{
		GPXDir:                     cmd.String("gpx-dir"),
		ElevationHost:              elevationHost,
		ElevationDataset:           cmd.String("elevation-dataset"),
		ElevationTiles:             elevationTiles,
		ElevationTileZoom:          tileZoom,
		ElevationTileCache:         tileCache,
		ElevationTileCacheMaxBytes: tileCacheBytes,
		NominatimURL:               cmd.String("nominatim-url"),
		OfflineCacheDir:            cmd.String("offline-cache-dir"),
		OfflineCacheMaxBytes:       cacheBytes,
		OfflineCacheMaxEntries:     cmd.Int("offline-cache-max-entries"),
		OfflineMode:                cmd.String("offline-mode"),
		UpstreamContact:            cmd.String("upstream-contact"),
		TrustedUIOrigin:            cmd.String("trusted-ui-origin"),
		OfflineAdminToken:          cmd.String("offline-admin-token"),
		ValhallaURL:                cmd.String("valhalla-url"),
		OSRMURL:                    cmd.String("osrm-url"),
		OverpassURL:                cmd.String("overpass-url"),
		FuelURL:                    cmd.String("fuel-url"),
		OpenFreeMapURL:             cmd.String("openfreemap-url"),
		OpenFreeMapAllowBulk:       cmd.Bool("openfreemap-allow-bulk"),
		AllowedOrigins:             cmd.StringSlice("allowed-origin"),
		Assets:                     assets,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	httpServer := &http.Server{
		Addr:              cmd.String("addr"),
		Handler:           logRequests(srv),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	elevationSource := "Open-Meteo (free non-commercial API, Copernicus 90 m)"
	switch {
	case elevationHost != "":
		elevationSource = fmt.Sprintf("%s (%s)", elevationHost, cmd.String("elevation-dataset"))
	case elevationTiles:
		if tileZoom <= 0 {
			tileZoom = 13
		}
		where := "memory only"
		if tileCache != "" {
			where = "cached in " + tileCache
		}
		elevationSource = fmt.Sprintf("terrain tiles z%d, %s", tileZoom, where)
	}

	log.Printf("%s %s listening on %s (library: %s, elevation: %s)",
		util.AppName, cmd.Root().Version, cmd.String("addr"), cmd.String("gpx-dir"), elevationSource)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		log.Printf("%s stopped", util.AppName)
		return nil
	}
}

func parseByteSize(value string) (int64, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return 0, errors.New("value is empty")
	}
	lower := strings.ToLower(raw)
	multiplier := int64(1)
	for suffix, scale := range map[string]int64{
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30,
		"kb": 1000, "mb": 1000 * 1000, "gb": 1000 * 1000 * 1000,
	} {
		if strings.HasSuffix(lower, suffix) {
			multiplier = scale
			raw = strings.TrimSpace(raw[:len(raw)-len(suffix)])
			break
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || n > (1<<63-1)/multiplier {
		return 0, errors.New("must be a positive byte count with optional KiB, MiB, or GiB suffix")
	}
	return n * multiplier, nil
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status >= 400 || !strings.HasPrefix(r.URL.Path, "/assets/") {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status,
				time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
