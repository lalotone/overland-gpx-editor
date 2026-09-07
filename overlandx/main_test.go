package main

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestParseOptions(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))

	opts, err := parseOptions([]string{
		"-gpx-dir", "/tracks",
		"-elevation-tiles=false",
		"-width", "1200",
		"-height", "800",
		"-debug",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseOptions() error = %v", err)
	}
	if opts.gpxDir != "/tracks" {
		t.Errorf("gpxDir = %q, want /tracks", opts.gpxDir)
	}
	if opts.elevationTiles {
		t.Error("elevationTiles = true, want false")
	}
	if opts.width != 1200 || opts.height != 800 {
		t.Errorf("window size = %dx%d, want 1200x800", opts.width, opts.height)
	}
	if !opts.debug {
		t.Error("debug = false, want true")
	}
}

func TestParseOptionsRejectsInvalidSize(t *testing.T) {
	_, err := parseOptions([]string{"-width", "0"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseOptions() error = nil, want invalid size error")
	}
}

func TestRunPrintsVersionWithoutOpeningWindow(t *testing.T) {
	oldVersion := version
	version = "test-version"
	t.Cleanup(func() { version = oldVersion })

	var stdout bytes.Buffer
	if err := run([]string{"-version"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := stdout.String(), "test-version\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestRunPrintsHelpWithoutOpeningWindow(t *testing.T) {
	var stderr bytes.Buffer
	if err := run([]string{"-help"}, &bytes.Buffer{}, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Usage of overlandx:")) {
		t.Errorf("help output = %q, want usage", stderr.String())
	}
}
