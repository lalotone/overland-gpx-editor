package serve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/lalotone/overland-gpx-editor/internal/mcp"
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
		&cli.BoolFlag{
			Name:    "mcp",
			Usage:   "enable the loopback-only Streamable HTTP MCP endpoint",
			Sources: util.BoolEnv("MCP"),
		},
		&cli.StringFlag{
			Name:    "mcp-addr",
			Usage:   "loopback address for the MCP endpoint; kept off the main listener so a reverse proxy cannot reach it",
			Value:   "127.0.0.1:8009",
			Sources: util.NonEmptyEnv("MCP_ADDR"),
		},
		&cli.BoolFlag{
			Name:    "behind-proxy",
			Usage:   "the server sits behind a reverse proxy; withdraws implicit loopback trust so management needs --offline-admin-token",
			Sources: util.BoolEnv("BEHIND_PROXY"),
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
		&cli.DurationFlag{Name: "stats-log-interval", Usage: "interval for privacy-safe aggregate cache and outbound stats (0 disables)", Value: time.Minute, Sources: util.NonEmptyEnv("STATS_LOG_INTERVAL")},
		&cli.StringFlag{Name: "upstream-contact", Usage: "operator contact included in outbound User-Agent", Value: "https://github.com/lalotone/overland-gpx-editor", Sources: util.NonEmptyEnv("UPSTREAM_CONTACT")},
		&cli.StringFlag{Name: "trusted-ui-origin", Usage: "exact remote UI origin allowed to manage offline data", Sources: util.NonEmptyEnv("TRUSTED_UI_ORIGIN")},
		&cli.StringFlag{Name: "offline-admin-token", Usage: "Bearer token for non-loopback offline management", Sources: util.StringEnv("OFFLINE_ADMIN_TOKEN")},
		&cli.StringFlag{Name: "routing-cache-dir", Usage: "Broom routing data directory; empty disables local routing", Value: util.DefaultRoutingCacheDir(), Sources: util.StringEnv("ROUTING_CACHE_DIR")},
		&cli.StringFlag{Name: "routing-region", Usage: "canonical Broom region to reopen or prepare", Sources: util.NonEmptyEnv("ROUTING_REGION")},
		&cli.StringFlag{Name: "routing-graph", Usage: "trusted application-owned Broom graph to open instead of a managed region", Sources: util.NonEmptyEnv("ROUTING_GRAPH")},
		&cli.BoolFlag{Name: "routing-prepare", Usage: "prepare --routing-region in the background when serving", Sources: util.BoolEnv("ROUTING_PREPARE")},
		&cli.BoolFlag{Name: "routing-update", Usage: "explicitly refresh --routing-region while preparing", Sources: util.BoolEnv("ROUTING_UPDATE")},
		&cli.IntFlag{Name: "routing-jobs", Usage: "parallel Broom graph preparation jobs", Value: 2, Sources: util.IntEnv("ROUTING_JOBS")},
		&cli.IntFlag{Name: "routing-concurrency", Usage: "maximum concurrent local route queries", Value: 4, Sources: util.IntEnv("ROUTING_CONCURRENCY")},
		&cli.DurationFlag{Name: "routing-timeout", Usage: "deadline for one local route query", Value: 45 * time.Second, Sources: util.NonEmptyEnv("ROUTING_TIMEOUT")},
		&cli.StringFlag{Name: "routing-index-url", Usage: "Broom geometry index mirror", Sources: util.NonEmptyEnv("ROUTING_INDEX_URL")},
		&cli.StringFlag{Name: "routing-metadata-index-url", Usage: "Broom metadata index mirror", Sources: util.NonEmptyEnv("ROUTING_METADATA_INDEX_URL")},
		&cli.StringFlag{Name: "routing-pbf-base-url", Usage: "Broom PBF/checksum mirror base", Sources: util.NonEmptyEnv("ROUTING_PBF_BASE_URL")},
		&cli.StringFlag{Name: "routing-dem-base-url", Usage: "Broom Skadi-compatible DEM mirror base", Sources: util.NonEmptyEnv("ROUTING_DEM_BASE_URL")},
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
	if (cmd.Bool("routing-prepare") || cmd.Bool("routing-update")) && strings.TrimSpace(cmd.String("routing-region")) == "" {
		return errors.New("--routing-prepare and --routing-update require --routing-region")
	}

	behindProxy := cmd.Bool("behind-proxy")
	bridge, mcpServer, mcpListener, err := startMCP(cmd, behindProxy)
	if err != nil {
		return err
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
		StatsLogInterval:           cmd.Duration("stats-log-interval"),
		UpstreamContact:            cmd.String("upstream-contact"),
		TrustedUIOrigin:            cmd.String("trusted-ui-origin"),
		OfflineAdminToken:          cmd.String("offline-admin-token"),
		RoutingCacheDir:            cmd.String("routing-cache-dir"),
		RoutingRegion:              cmd.String("routing-region"),
		RoutingGraph:               cmd.String("routing-graph"),
		RoutingPrepare:             cmd.Bool("routing-prepare"),
		RoutingUpdate:              cmd.Bool("routing-update"),
		RoutingJobs:                cmd.Int("routing-jobs"),
		RoutingConcurrency:         cmd.Int("routing-concurrency"),
		RoutingTimeout:             cmd.Duration("routing-timeout"),
		RoutingIndexURL:            cmd.String("routing-index-url"),
		RoutingMetadataIndexURL:    cmd.String("routing-metadata-index-url"),
		RoutingPBFBaseURL:          cmd.String("routing-pbf-base-url"),
		RoutingDEMBaseURL:          cmd.String("routing-dem-base-url"),
		OverpassURL:                cmd.String("overpass-url"),
		FuelURL:                    cmd.String("fuel-url"),
		OpenFreeMapURL:             cmd.String("openfreemap-url"),
		OpenFreeMapAllowBulk:       cmd.Bool("openfreemap-allow-bulk"),
		AllowedOrigins:             cmd.StringSlice("allowed-origin"),
		MCPBrowserHandler:          browserBridge(bridge),
		BehindProxy:                behindProxy,
		Assets:                     assets,
	})
	if err != nil {
		closeMCP(bridge, mcpListener)
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
	httpListener, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		closeMCP(bridge, mcpListener)
		return err
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
	if cmd.String("routing-cache-dir") != "" {
		log.Printf("Broom routing enabled (data: %s, region: %s)", cmd.String("routing-cache-dir"), valueOrNone(cmd.String("routing-region")))
	}
	if bridge != nil {
		log.Printf("MCP available at http://%s/mcp (Streamable HTTP, loopback clients only)", mcpListener.Addr())
	}
	if behindProxy {
		log.Printf("behind a reverse proxy: loopback trust withdrawn, offline management requires an admin token")
		// Without a declared origin the relay refuses the proxied frontend's
		// own requests, which looks like routing and tiles quietly breaking.
		if len(cmd.StringSlice("allowed-origin")) == 0 && strings.TrimSpace(cmd.String("trusted-ui-origin")) == "" {
			log.Printf("warning: --behind-proxy without --allowed-origin or --trusted-ui-origin will refuse " +
				"the proxied frontend's routing, elevation and map requests")
		}
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	type serviceResult struct {
		name string
		err  error
	}
	serviceCount := 1
	if mcpServer != nil {
		serviceCount++
	}
	errCh := make(chan serviceResult, serviceCount)
	go func() { errCh <- serviceResult{"HTTP server", httpServer.Serve(httpListener)} }()
	if mcpServer != nil {
		go func() { errCh <- serviceResult{"MCP server", mcpServer.Serve(mcpListener)} }()
	}

	var first serviceResult
	firstReceived := false
	select {
	case first = <-errCh:
		firstReceived = true
	case <-ctx.Done():
	}

	if bridge != nil {
		bridge.Close()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = httpServer.Close()
	}
	if mcpServer != nil {
		if err := mcpServer.Shutdown(shutdownCtx); err != nil {
			_ = mcpServer.Close()
		}
	}

	remaining := serviceCount
	if firstReceived {
		remaining--
	}
	failure := serviceResult{}
	if firstReceived && !expectedServiceStop(first.err) {
		failure = first
	}
	for range remaining {
		result := <-errCh
		if failure.err == nil && !expectedServiceStop(result.err) {
			failure = result
		}
	}
	if failure.err != nil {
		return fmt.Errorf("%s: %w", failure.name, failure.err)
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}
	log.Printf("%s stopped", util.AppName)
	return nil
}

func expectedServiceStop(err error) bool {
	return errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)
}

func valueOrNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "select in planner"
	}
	return strings.TrimSpace(value)
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
		if !routineMCPBrowserSuccess(r, rec.status) && (rec.status >= 400 || !strings.HasPrefix(r.URL.Path, "/assets/")) {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status,
				time.Since(start).Round(time.Millisecond))
		}
	})
}

func routineMCPBrowserSuccess(r *http.Request, status int) bool {
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/mcp/browser/view":
		return status == http.StatusNoContent
	// One line per opened stream would otherwise be logged on every reconnect.
	case r.Method == http.MethodGet && r.URL.Path == "/mcp/browser/events":
		return status == http.StatusOK
	default:
		return false
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the real writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Flush keeps the MCP browser event stream working. Chi's middleware decides
// whether its own wrapper supports flushing by type-asserting for
// http.Flusher, so a recorder that only implements Unwrap makes the whole
// chain non-flushing and commands sit in a buffer until the tab closes.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// browserBridge returns the private bridge handler, or a nil interface when
// MCP is disabled so no MCP route is registered at all.
func browserBridge(bridge *mcp.Bridge) http.Handler {
	if bridge == nil {
		return nil
	}
	return bridge
}

func closeMCP(bridge *mcp.Bridge, listener net.Listener) {
	if listener != nil {
		_ = listener.Close()
	}
	if bridge != nil {
		bridge.Close()
	}
}

// requireLoopbackAddr refuses to expose the agent endpoint beyond this
// machine. It is the guarantee a reverse proxy cannot undo with a header.
func requireLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("must be host:port")
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("must be a loopback address")
	}
	return nil
}

// startMCP prepares the agent-facing MCP endpoint on its own loopback
// listener. It returns nils when --mcp is not set.
func startMCP(cmd *cli.Command, behindProxy bool) (*mcp.Bridge, *http.Server, net.Listener, error) {
	if !cmd.Bool("mcp") {
		return nil, nil, nil, nil
	}
	// A proxied deployment serves remote browsers, so the tab an agent would
	// drive is not on this machine and the loopback guarantees the bridge
	// depends on no longer hold. Refuse rather than half-enable it.
	if behindProxy {
		return nil, nil, nil, errors.New("--mcp cannot be combined with --behind-proxy: MCP controls a browser on this machine only")
	}
	addr := cmd.String("mcp-addr")
	if err := requireLoopbackAddr(addr); err != nil {
		return nil, nil, nil, fmt.Errorf("mcp-addr: %w", err)
	}
	bridge, err := mcp.NewBridge()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create MCP bridge: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.StripPrefix("/mcp", mcp.NewHandler(bridge, cmd.Root().Version)))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		bridge.Close()
		return nil, nil, nil, err
	}
	return bridge, &http.Server{
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}, listener, nil
}
