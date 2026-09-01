package serve

import (
	"context"
	"testing"
	"time"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/urfave/cli/v3"
)

func TestEmptyEnvironmentValuesUseDefaults(t *testing.T) {
	for _, key := range []string{
		"ADDR", "GPX_DIR", "ELEVATION_HOST", "ELEVATION_DATASET",
		"ELEVATION_TILES", "ELEVATION_TILE_ZOOM", "ELEVATION_TILE_CACHE",
		"ELEVATION_TILE_CACHE_MAX_BYTES",
		"NOMINATIM_URL", "OPENFREEMAP_URL", "OPENFREEMAP_ALLOW_BULK", "ALLOWED_ORIGINS", "STATS_LOG_INTERVAL",
	} {
		t.Setenv(key, "")
	}

	cmd := &cli.Command{
		Flags: Flags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			if got := cmd.String("addr"); got != "127.0.0.1:8000" {
				t.Errorf("addr = %q, want 127.0.0.1:8000", got)
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
