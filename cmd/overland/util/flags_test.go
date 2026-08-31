package util

import (
	"path/filepath"
	"testing"
)

func TestDefaultDirsUseXDG(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "data")
	cacheHome := filepath.Join(t.TempDir(), "cache")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	if got, want := DefaultGPXDir(), filepath.Join(dataHome, "overland", "gpx"); got != want {
		t.Errorf("DefaultGPXDir() = %q, want %q", got, want)
	}
	if got, want := DefaultTileCacheDir(), filepath.Join(cacheHome, "overland", "tiles"); got != want {
		t.Errorf("DefaultTileCacheDir() = %q, want %q", got, want)
	}
	if got, want := DefaultOfflineCacheDir(), filepath.Join(cacheHome, "overland", "responses"); got != want {
		t.Errorf("DefaultOfflineCacheDir() = %q, want %q", got, want)
	}
}

func TestDefaultDirsFallBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	if got, want := DefaultGPXDir(), filepath.Join(home, ".local", "share", "overland", "gpx"); got != want {
		t.Errorf("DefaultGPXDir() = %q, want %q", got, want)
	}
	if got, want := DefaultTileCacheDir(), filepath.Join(home, ".cache", "overland", "tiles"); got != want {
		t.Errorf("DefaultTileCacheDir() = %q, want %q", got, want)
	}
	if got, want := DefaultOfflineCacheDir(), filepath.Join(home, ".cache", "overland", "responses"); got != want {
		t.Errorf("DefaultOfflineCacheDir() = %q, want %q", got, want)
	}
}

func TestDefaultDirsIgnoreRelativeXDGPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "relative-data")
	t.Setenv("XDG_CACHE_HOME", "relative-cache")

	if got, want := DefaultGPXDir(), filepath.Join(home, ".local", "share", "overland", "gpx"); got != want {
		t.Errorf("DefaultGPXDir() = %q, want %q", got, want)
	}
	if got, want := DefaultTileCacheDir(), filepath.Join(home, ".cache", "overland", "tiles"); got != want {
		t.Errorf("DefaultTileCacheDir() = %q, want %q", got, want)
	}
}

func TestNonEmptyEnv(t *testing.T) {
	t.Setenv("OVERLAND_TEST_STRING", "")
	source := NonEmptyEnv("OVERLAND_TEST_STRING")
	if value, ok := source.Lookup(); ok {
		t.Fatalf("empty environment value resolved as %q", value)
	}

	t.Setenv("OVERLAND_TEST_STRING", "value")
	source = NonEmptyEnv("OVERLAND_TEST_STRING")
	if value, ok := source.Lookup(); !ok || value != "value" {
		t.Fatalf("environment lookup = %q, %v; want value, true", value, ok)
	}
}

func TestStringEnvPreservesExplicitEmpty(t *testing.T) {
	t.Setenv("OVERLAND_TEST_STRING", "")
	source := StringEnv("OVERLAND_TEST_STRING")
	value, ok := source.Lookup()
	if !ok || value != "" {
		t.Fatalf("empty lookup = %q, %v; want empty, true", value, ok)
	}
}

func TestBoolEnvCompatibility(t *testing.T) {
	tests := []struct {
		value   string
		want    string
		wantSet bool
	}{
		{value: "", wantSet: false},
		{value: "0", want: "false", wantSet: true},
		{value: "false", want: "false", wantSet: true},
		{value: " no ", want: "false", wantSet: true},
		{value: "OFF", want: "false", wantSet: true},
		{value: "yes", want: "true", wantSet: true},
		{value: "anything", want: "true", wantSet: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("OVERLAND_TEST_BOOL", tt.value)
			source := BoolEnv("OVERLAND_TEST_BOOL")
			got, ok := source.Lookup()
			if got != tt.want || ok != tt.wantSet {
				t.Fatalf("lookup = %q, %v; want %q, %v", got, ok, tt.want, tt.wantSet)
			}
		})
	}
}

func TestIntEnvCompatibility(t *testing.T) {
	tests := []struct {
		value   string
		wantSet bool
	}{
		{value: "", wantSet: false},
		{value: "invalid", wantSet: false},
		{value: "13", wantSet: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("OVERLAND_TEST_INT", tt.value)
			source := IntEnv("OVERLAND_TEST_INT")
			got, ok := source.Lookup()
			if ok != tt.wantSet || ok && got != tt.value {
				t.Fatalf("lookup = %q, %v; want %q, %v", got, ok, tt.value, tt.wantSet)
			}
		})
	}
}
