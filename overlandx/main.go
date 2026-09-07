package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/crgimenes/glaze"
	"github.com/lalotone/overland-gpx-editor"
)

var version = "dev"

type options struct {
	gpxDir             string
	elevationHost      string
	elevationDataset   string
	elevationTiles     bool
	elevationTileCache string
	nominatimURL       string
	width              int
	height             int
	debug              bool
	showVersion        bool
}

func main() {
	log.SetFlags(0)
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if opts.showVersion {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}

	cfg := overland.DefaultConfig()
	cfg.GPXDir = opts.gpxDir
	cfg.ElevationHost = opts.elevationHost
	cfg.ElevationDataset = opts.elevationDataset
	cfg.ElevationTiles = opts.elevationTiles
	cfg.ElevationTileCache = opts.elevationTileCache
	cfg.NominatimURL = opts.nominatimURL

	app, err := overland.New(cfg)
	if err != nil {
		return fmt.Errorf("start Overland: %w", err)
	}

	windowErr := glaze.AppWindow(glaze.AppOptions{
		Title:     "OverlandX",
		Width:     opts.width,
		Height:    opts.height,
		Debug:     opts.debug,
		Transport: glaze.AppTransportTCP,
		Handler:   app,
	})
	closeErr := app.Close()
	if windowErr != nil {
		windowErr = fmt.Errorf("open OverlandX window: %w", windowErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close Overland: %w", closeErr)
	}
	return errors.Join(windowErr, closeErr)
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	defaults := overland.DefaultConfig()
	opts := options{}
	flags := flag.NewFlagSet("overlandx", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.gpxDir, "gpx-dir", defaults.GPXDir, "directory holding the track library")
	flags.StringVar(&opts.elevationHost, "elevation-host", defaults.ElevationHost, "self-hosted opentopodata-style DEM service")
	flags.StringVar(&opts.elevationDataset, "elevation-dataset", defaults.ElevationDataset, "dataset used by the configured elevation host")
	flags.BoolVar(&opts.elevationTiles, "elevation-tiles", defaults.ElevationTiles, "read elevation from terrain-RGB tiles")
	flags.StringVar(&opts.elevationTileCache, "elevation-tile-cache", defaults.ElevationTileCache, "directory for cached terrain tiles")
	flags.StringVar(&opts.nominatimURL, "nominatim-url", defaults.NominatimURL, "Nominatim-compatible place search service")
	flags.IntVar(&opts.width, "width", 1400, "initial window width")
	flags.IntVar(&opts.height, "height", 900, "initial window height")
	flags.BoolVar(&opts.debug, "debug", false, "enable WebView developer tools")
	flags.BoolVar(&opts.showVersion, "version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if opts.width <= 0 || opts.height <= 0 {
		return options{}, errors.New("width and height must be positive")
	}
	return opts, nil
}
