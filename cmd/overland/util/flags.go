package util

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"
)

const AppName = "overland"

func GPXDirFlag() cli.Flag {
	return &cli.StringFlag{
		Name:    "gpx-dir",
		Usage:   "directory holding the track library",
		Value:   DefaultGPXDir(),
		Sources: NonEmptyEnv("GPX_DIR"),
	}
}

func NonEmptyEnv(key string) cli.ValueSourceChain {
	return envSource(key, func(value string) (string, bool) {
		return value, value != ""
	})
}

func BoolEnv(key string) cli.ValueSourceChain {
	return envSource(key, func(value string) (string, bool) {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "":
			return "", false
		case "0", "false", "no", "off":
			return "false", true
		default:
			return "true", true
		}
	})
}

func IntEnv(key string) cli.ValueSourceChain {
	return envSource(key, func(value string) (string, bool) {
		if _, err := strconv.Atoi(value); err != nil {
			return "", false
		}
		return value, true
	})
}

func envSource(key string, normalize func(string) (string, bool)) cli.ValueSourceChain {
	return cli.NewValueSourceChain(&environmentSource{key: key, normalize: normalize})
}

type environmentSource struct {
	key       string
	normalize func(string) (string, bool)
}

func (s *environmentSource) Lookup() (string, bool) {
	value, ok := os.LookupEnv(s.key)
	if !ok {
		return "", false
	}
	return s.normalize(value)
}

func (s *environmentSource) IsFromEnv() bool { return true }
func (s *environmentSource) Key() string     { return s.key }
func (s *environmentSource) String() string  { return fmt.Sprintf("environment variable %q", s.key) }
func (s *environmentSource) GoString() string {
	return fmt.Sprintf("&environmentSource{key:%q}", s.key)
}

func DefaultGPXDir() string {
	if base := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(base) {
		return filepath.Join(base, "overland", "gpx")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "gpx"
	}
	return filepath.Join(home, ".local", "share", "overland", "gpx")
}

func DefaultTileCacheDir() string {
	if base := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(base) {
		return filepath.Join(base, "overland", "tiles")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "tiles"
	}
	return filepath.Join(home, ".cache", "overland", "tiles")
}
