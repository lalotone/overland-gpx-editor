package serve

import (
	"context"
	"testing"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/urfave/cli/v3"
)

func TestEmptyEnvironmentValuesUseDefaults(t *testing.T) {
	for _, key := range []string{
		"ADDR", "GPX_DIR", "ELEVATION_HOST", "ELEVATION_DATASET",
		"ELEVATION_TILES", "ELEVATION_TILE_ZOOM", "ELEVATION_TILE_CACHE",
		"NOMINATIM_URL", "ALLOWED_ORIGINS",
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
			if got := cmd.String("nominatim-url"); got != "https://nominatim.openstreetmap.org" {
				t.Errorf("nominatim-url = %q", got)
			}
			if got := cmd.StringSlice("allowed-origin"); len(got) != 0 {
				t.Errorf("allowed-origin = %v, want empty", got)
			}
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatal(err)
	}
}

func TestBooleanEnvironmentCompatibility(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ELEVATION_TILES", value)
			cmd := &cli.Command{
				Flags: Flags(),
				Action: func(_ context.Context, cmd *cli.Command) error {
					if cmd.Bool("elevation-tiles") {
						t.Errorf("ELEVATION_TILES=%q resolved to true", value)
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
