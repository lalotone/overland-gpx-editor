package serve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/lalotone/overland-gpx-editor/internal/server"
	"github.com/lalotone/overland-gpx-editor/web"
	"github.com/urfave/cli/v3"
)

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
			Value:   ":8000",
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
			Name:    "nominatim-url",
			Usage:   "Nominatim-compatible place-search URL exposed to the frontend",
			Value:   "https://nominatim.openstreetmap.org",
			Sources: util.NonEmptyEnv("NOMINATIM_URL"),
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
	srv, err := server.New(server.Config{
		GPXDir:             cmd.String("gpx-dir"),
		ElevationHost:      elevationHost,
		ElevationDataset:   cmd.String("elevation-dataset"),
		ElevationTiles:     elevationTiles,
		ElevationTileZoom:  tileZoom,
		ElevationTileCache: tileCache,
		NominatimURL:       cmd.String("nominatim-url"),
		Assets:             assets,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	httpServer := &http.Server{
		Addr:    cmd.String("addr"),
		Handler: logRequests(srv),
		// Uploads and DEM proxying can be slow; only the header deadline is
		// safe to keep tight.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
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
