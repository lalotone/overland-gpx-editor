package importcmd

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/identity"
	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/lalotone/overland-gpx-editor/internal/passkeyauth"
	"github.com/lalotone/overland-gpx-editor/internal/server"
	"github.com/urfave/cli/v3"
)

var Command = &cli.Command{
	Name:      "import",
	Usage:     "Import GPX files into a track library",
	ArgsUsage: "FILE...",
	Flags: []cli.Flag{
		util.DataDirFlag(),
		&cli.StringFlag{
			Name:  "user",
			Usage: "import into this account's library (serve --auth); the default is the library serve uses without --auth",
		},
		util.AuthDBFlag(),
	},
	Action: run,
}

func run(_ context.Context, cmd *cli.Command) error {
	paths := cmd.Args().Slice()
	if len(paths) == 0 {
		return errors.New("at least one GPX file is required")
	}
	owner, err := resolveOwner(cmd.String("user"), cmd.String("auth-db"))
	if err != nil {
		return err
	}
	root, err := server.OpenOwnerTracks(cmd.String("data-dir"), owner)
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

// resolveOwner maps --user to the owner key the server stores that account's
// library under. The lookup stays here, in the CLI: the account database and
// owner key are opened together, and only the derived key goes any further.
// An unknown or disabled user is an error, never a fallback to the local
// library, which would quietly hand their files to a different owner.
func resolveOwner(username, authDB string) (server.Owner, error) {
	if username == "" {
		return server.LocalOwner, nil
	}
	store, err := passkeyauth.OpenStore(authDB)
	if err != nil {
		return "", fmt.Errorf("open account database: %w", err)
	}
	defer store.Close()
	account, err := store.UserByName(username)
	if err != nil {
		return "", fmt.Errorf("user %q: %w", username, err)
	}
	if account.Disabled {
		return "", fmt.Errorf("user %q is disabled", username)
	}
	secret, err := identity.Load(identity.KeyPath(authDB))
	if err != nil {
		return "", fmt.Errorf("owner key: %w (it is created by `%s serve --auth`)", err, util.AppName)
	}
	return secret.Owner(account)
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
