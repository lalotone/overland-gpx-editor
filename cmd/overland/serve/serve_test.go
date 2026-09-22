package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/lalotone/overland-gpx-editor/internal/mcp"
	"github.com/lalotone/overland-gpx-editor/internal/server"
	"github.com/urfave/cli/v3"
)

// The command stream has to survive every response wrapper in the real stack:
// the request logger, chi's compressor and the throttle. If any of them hides
// the underlying flusher, commands sit in a buffer until the tab closes.
func TestMCPBrowserStreamFlushesThroughTheServerStack(t *testing.T) {
	bridge, err := mcp.NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	srv, err := server.New(server.Config{
		GPXDir:            t.TempDir(),
		ElevationHost:     "http://elevation.invalid",
		MCPBrowserHandler: bridge,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	httpServer := httptest.NewServer(logRequests(srv))
	t.Cleanup(httpServer.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	session, err := client.Get(httpServer.URL + "/mcp/browser/session")
	if err != nil {
		t.Fatal(err)
	}
	session.Body.Close()
	if session.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d", session.StatusCode)
	}

	view := `{"viewId":"view-one","sequence":1,"active":true,"snapshot":{"screen":"creation"}}`
	request, err := http.NewRequest(http.MethodPut, httpServer.URL+"/mcp/browser/view", strings.NewReader(view))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	published, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	published.Body.Close()
	if published.StatusCode != http.StatusNoContent {
		t.Fatalf("view status = %d", published.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	streamRequest, err := http.NewRequestWithContext(ctx, http.MethodGet,
		httpServer.URL+"/mcp/browser/events?view_id=view-one", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Ask for compression the way a browser does, so a compressor that
	// buffered the stream would show up here.
	streamRequest.Header.Set("Accept-Encoding", "gzip")
	stream, err := client.Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", stream.StatusCode)
	}
	if got := stream.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("stream content type = %q", got)
	}

	go func() {
		_, _ = bridge.Call(ctx, "set_map_view", json.RawMessage(`{"lat":42.85,"lon":-2.67}`))
	}()

	frames := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(stream.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(frames)
				return
			}
			if payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: "); ok {
				frames <- payload
				return
			}
		}
	}()

	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatal("event stream closed before delivering a command")
		}
		if !strings.Contains(frame, `"name":"set_map_view"`) {
			t.Fatalf("streamed frame = %s", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command was buffered instead of flushed to the stream")
	}
}

func TestEmptyEnvironmentValuesUseDefaults(t *testing.T) {
	for _, key := range []string{
		"ADDR", "MCP", "GPX_DIR", "ELEVATION_HOST", "ELEVATION_DATASET",
		"ELEVATION_TILES", "ELEVATION_TILE_ZOOM", "ELEVATION_TILE_CACHE",
		"ELEVATION_TILE_CACHE_MAX_BYTES",
		"NOMINATIM_URL", "OPENFREEMAP_URL", "OPENFREEMAP_ALLOW_BULK", "ALLOWED_ORIGINS", "STATS_LOG_INTERVAL",
		"ROUTING_CACHE_DIR", "ROUTING_REGION", "ROUTING_GRAPH", "ROUTING_PREPARE", "ROUTING_UPDATE",
		"ROUTING_JOBS", "ROUTING_CONCURRENCY", "ROUTING_TIMEOUT", "ROUTING_INDEX_URL",
		"ROUTING_METADATA_INDEX_URL", "ROUTING_PBF_BASE_URL", "ROUTING_DEM_BASE_URL",
	} {
		t.Setenv(key, "")
	}

	cmd := &cli.Command{
		Flags: Flags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			if got := cmd.String("addr"); got != "127.0.0.1:8000" {
				t.Errorf("addr = %q, want 127.0.0.1:8000", got)
			}
			if cmd.Bool("mcp") {
				t.Error("mcp = true, want false")
			}
			if got := cmd.String("gpx-dir"); got != util.DefaultGPXDir() {
				t.Errorf("gpx-dir = %q, want %q", got, util.DefaultGPXDir())
			}
			if got := cmd.String("elevation-dataset"); got != "srtm30m" {
				t.Errorf("elevation-dataset = %q, want srtm30m", got)
			}
			if !cmd.Bool("elevation-tiles") {
				t.Error("elevation-tiles = false, want true")
			}
			if got := cmd.Int("elevation-tile-zoom"); got != 0 {
				t.Errorf("elevation-tile-zoom = %d, want 0", got)
			}
			if got := cmd.String("elevation-tile-cache"); got != util.DefaultTileCacheDir() {
				t.Errorf("elevation-tile-cache = %q, want %q", got, util.DefaultTileCacheDir())
			}
			if got := cmd.String("elevation-tile-cache-max-bytes"); got != "1GiB" {
				t.Errorf("elevation-tile-cache-max-bytes = %q, want 1GiB", got)
			}
			if got := cmd.String("nominatim-url"); got != "https://nominatim.openstreetmap.org" {
				t.Errorf("nominatim-url = %q", got)
			}
			if got := cmd.String("openfreemap-url"); got != defaultOpenFreeMapURL {
				t.Errorf("openfreemap-url = %q, want %q", got, defaultOpenFreeMapURL)
			}
			if got := cmd.String("routing-cache-dir"); got != "" {
				t.Errorf("routing-cache-dir = %q, want explicit empty environment value", got)
			}
			if got := cmd.Int("routing-jobs"); got != 2 {
				t.Errorf("routing-jobs = %d, want 2", got)
			}
			if got := cmd.Int("routing-concurrency"); got != 4 {
				t.Errorf("routing-concurrency = %d, want 4", got)
			}
			if got := cmd.Duration("routing-timeout"); got != 45*time.Second {
				t.Errorf("routing-timeout = %s, want 45s", got)
			}
			if !cmd.Bool("openfreemap-allow-bulk") {
				t.Error("openfreemap-allow-bulk = false, want true")
			}
			if got := cmd.StringSlice("allowed-origin"); len(got) != 0 {
				t.Errorf("allowed-origin = %v, want empty", got)
			}
			if got := cmd.Duration("stats-log-interval"); got != time.Minute {
				t.Errorf("stats-log-interval = %s, want 1m", got)
			}
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatal(err)
	}
}

func TestElevationTileCacheQuotaEnvironment(t *testing.T) {
	t.Setenv("ELEVATION_TILE_CACHE_MAX_BYTES", "256MiB")
	cmd := &cli.Command{Flags: Flags(), Action: func(_ context.Context, cmd *cli.Command) error {
		if got := cmd.String("elevation-tile-cache-max-bytes"); got != "256MiB" {
			t.Errorf("elevation-tile-cache-max-bytes = %q", got)
		}
		return nil
	}}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatal(err)
	}
}

func TestStatsLogIntervalEnvironment(t *testing.T) {
	t.Setenv("STATS_LOG_INTERVAL", "15s")
	cmd := &cli.Command{Flags: Flags(), Action: func(_ context.Context, cmd *cli.Command) error {
		if got := cmd.Duration("stats-log-interval"); got != 15*time.Second {
			t.Errorf("stats-log-interval = %s", got)
		}
		return nil
	}}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatal(err)
	}
}

func TestMCPEnvironment(t *testing.T) {
	t.Setenv("MCP", "true")
	cmd := &cli.Command{Flags: Flags(), Action: func(_ context.Context, cmd *cli.Command) error {
		if !cmd.Bool("mcp") {
			t.Error("mcp = false, want true")
		}
		return nil
	}}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatal(err)
	}
}

func TestBooleanEnvironmentCompatibility(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ELEVATION_TILES", value)
			t.Setenv("OPENFREEMAP_ALLOW_BULK", value)
			cmd := &cli.Command{
				Flags: Flags(),
				Action: func(_ context.Context, cmd *cli.Command) error {
					if cmd.Bool("elevation-tiles") {
						t.Errorf("ELEVATION_TILES=%q resolved to true", value)
					}
					if cmd.Bool("openfreemap-allow-bulk") {
						t.Errorf("OPENFREEMAP_ALLOW_BULK=%q resolved to true", value)
					}
					return nil
				},
			}
			if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidIntegerEnvironmentUsesDefault(t *testing.T) {
	t.Setenv("ELEVATION_TILE_ZOOM", "invalid")
	cmd := &cli.Command{
		Flags: Flags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			if got := cmd.Int("elevation-tile-zoom"); got != 0 {
				t.Errorf("elevation-tile-zoom = %d, want 0", got)
			}
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedOriginsEnvironment(t *testing.T) {
	t.Setenv("ALLOWED_ORIGINS", "https://one.example,https://two.example")
	cmd := &cli.Command{
		Flags: Flags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			got := cmd.StringSlice("allowed-origin")
			if len(got) != 2 || got[0] != "https://one.example" || got[1] != "https://two.example" {
				t.Errorf("allowed-origin = %v", got)
			}
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineFlagDefaultsAndExplicitDisable(t *testing.T) {
	t.Setenv("OFFLINE_CACHE_DIR", "")
	cmd := &cli.Command{Flags: Flags(), Action: func(_ context.Context, cmd *cli.Command) error {
		if got := cmd.String("offline-cache-dir"); got != "" {
			t.Errorf("offline-cache-dir = %q", got)
		}
		if got := cmd.String("offline-cache-max-bytes"); got != "1GiB" {
			t.Errorf("max bytes = %q", got)
		}
		if got := cmd.Int("offline-cache-max-entries"); got != 100000 {
			t.Errorf("max entries = %d", got)
		}
		if got := cmd.String("offline-mode"); got != "auto" {
			t.Errorf("mode = %q", got)
		}
		return nil
	}}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatal(err)
	}
}

func TestParseByteSize(t *testing.T) {
	tests := map[string]int64{"1": 1, "1KiB": 1 << 10, "2MiB": 2 << 20, "1GiB": 1 << 30, "2MB": 2_000_000}
	for input, want := range tests {
		got, err := parseByteSize(input)
		if err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0", "-1", "wat", "999999999999999999999GiB"} {
		if _, err := parseByteSize(input); err == nil {
			t.Errorf("parseByteSize(%q) accepted", input)
		}
	}
}

func TestLogRequestsSuppressesOnlyRoutineMCPBrowserSuccesses(t *testing.T) {
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	originalPrefix := log.Prefix()
	var output bytes.Buffer
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(originalWriter)
		log.SetFlags(originalFlags)
		log.SetPrefix(originalPrefix)
	})

	tests := []struct {
		name   string
		method string
		path   string
		status int
		logged bool
	}{
		{name: "opened command stream", method: http.MethodGet, path: "/mcp/browser/events?view_id=one", status: http.StatusOK},
		{name: "view heartbeat", method: http.MethodPut, path: "/mcp/browser/view", status: http.StatusNoContent},
		{name: "rejected command stream", method: http.MethodGet, path: "/mcp/browser/events", status: http.StatusUnauthorized, logged: true},
		{name: "failed heartbeat", method: http.MethodPut, path: "/mcp/browser/view", status: http.StatusBadRequest, logged: true},
		{name: "command result", method: http.MethodPost, path: "/mcp/browser/result", status: http.StatusNoContent, logged: true},
		{name: "standard MCP", method: http.MethodPost, path: "/mcp", status: http.StatusOK, logged: true},
		{name: "ordinary request", method: http.MethodGet, path: "/healthz", status: http.StatusNoContent, logged: true},
		{name: "non-exact stream path", method: http.MethodGet, path: "/mcp/browser/events/", status: http.StatusOK, logged: true},
		{name: "successful asset", method: http.MethodGet, path: "/assets/app.js", status: http.StatusOK},
		{name: "failed asset", method: http.MethodGet, path: "/assets/missing.js", status: http.StatusNotFound, logged: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output.Reset()
			handler := logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(test.method, test.path, nil))
			if got := output.Len() > 0; got != test.logged {
				t.Errorf("logged = %t, want %t; output %q", got, test.logged, output.String())
			}
		})
	}
}

func TestRequireLoopbackAddrRefusesExposedMCP(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8009", "localhost:8009", "[::1]:8009"} {
		if err := requireLoopbackAddr(addr); err != nil {
			t.Errorf("requireLoopbackAddr(%q) = %v, want nil", addr, err)
		}
	}
	// Anything a proxy or another machine could reach must be refused at
	// startup; the socket is the only guarantee a header cannot undo.
	for _, addr := range []string{"0.0.0.0:8009", ":8009", "192.168.1.10:8009", "8009", "example.test:8009"} {
		if err := requireLoopbackAddr(addr); err == nil {
			t.Errorf("requireLoopbackAddr(%q) accepted", addr)
		}
	}
}

func TestMCPRefusesToRunBehindAProxy(t *testing.T) {
	cmd := &cli.Command{Name: "serve", Flags: Flags(), Action: Run}
	err := cmd.Run(context.Background(), []string{"serve", "--mcp", "--behind-proxy", "--gpx-dir", t.TempDir()})
	if err == nil {
		t.Fatal("MCP started behind a reverse proxy")
	}
	if !strings.Contains(err.Error(), "--behind-proxy") {
		t.Fatalf("error = %v, want it to name the conflicting flag", err)
	}
}
