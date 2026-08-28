package importcmd

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/lalotone/overland-gpx-editor/internal/server"
	"github.com/urfave/cli/v3"
)

var Command = &cli.Command{
	Name:      "import",
	Usage:     "Import GPX files into the track library",
	ArgsUsage: "FILE...",
	Flags: []cli.Flag{
		util.GPXDirFlag(),
	},
	Action: run,
}

func run(_ context.Context, cmd *cli.Command) error {
	paths := cmd.Args().Slice()
	if len(paths) == 0 {
		return errors.New("at least one GPX file is required")
	}

	dir := cmd.String("gpx-dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create track library: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open track library: %w", err)
	}
	defer root.Close()
	for _, path := range paths {
		filename, err := importFile(root, path)
		if err != nil {
			return fmt.Errorf("import %q: %w", path, err)
		}
		fmt.Fprintf(cmd.Writer, "Imported %s\n", filename)
	}
	return nil
}

func importFile(root *os.Root, sourcePath string) (string, error) {
	filename := filepath.Base(sourcePath)
	if err := server.ValidateGPXFilename(filename); err != nil {
		return "", err
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("source is not a regular file")
	}

	tmpName := ".import-" + rand.Text() + ".gpx"
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	defer root.Remove(tmpName)

	if _, err := io.Copy(tmp, source); err != nil {
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := root.Link(tmpName, filename); errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("%s already exists in the track library", filename)
	} else if err != nil {
		return "", fmt.Errorf("publish import: %w", err)
	}
	return filename, nil
}
