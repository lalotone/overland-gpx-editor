package main

import (
	"path/filepath"
	"testing"
)

func TestDefaultDirsUseXDG(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "data")
	cacheHome := filepath.Join(t.TempDir(), "cache")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	if got, want := defaultGPXDir(), filepath.Join(dataHome, "overland"); got != want {
		t.Errorf("defaultGPXDir() = %q, want %q", got, want)
	}
	if got, want := defaultTileCacheDir(), filepath.Join(cacheHome, "overland"); got != want {
		t.Errorf("defaultTileCacheDir() = %q, want %q", got, want)
	}
}

func TestDefaultDirsFallBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	if got, want := defaultGPXDir(), filepath.Join(home, ".local", "share", "overland"); got != want {
		t.Errorf("defaultGPXDir() = %q, want %q", got, want)
	}
	if got, want := defaultTileCacheDir(), filepath.Join(home, ".cache", "overland"); got != want {
		t.Errorf("defaultTileCacheDir() = %q, want %q", got, want)
	}
}

func TestDefaultDirsIgnoreRelativeXDGPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "relative-data")
	t.Setenv("XDG_CACHE_HOME", "relative-cache")

	if got, want := defaultGPXDir(), filepath.Join(home, ".local", "share", "overland"); got != want {
		t.Errorf("defaultGPXDir() = %q, want %q", got, want)
	}
	if got, want := defaultTileCacheDir(), filepath.Join(home, ".cache", "overland"); got != want {
		t.Errorf("defaultTileCacheDir() = %q, want %q", got, want)
	}
}
