package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Private data is laid out by owner under the data directory:
//
//	<data>/owners/<owner>/tracks/*.gpx     the GPX library
//	<data>/owners/<owner>/packs/<id>.json  trip-pack manifests
//	<data>/owners/<owner>/cache/           owner-scoped provider responses
//	<data>/owners/<owner>/tmp/             staging for atomic writes
//
// Ownership comes from the location. Another owner's data is under a
// different root, not behind a filter a handler could forget, and removing an
// owner is removing one directory. The shared response cache, terrain tiles
// and routing data live elsewhere and belong to nobody.
const (
	ownersDir       = "owners"
	ownerTracksDir  = "tracks"
	ownerPacksDir   = "packs"
	ownerCacheDir   = "cache"
	ownerTmpDir     = "tmp"
	ownerDirMode    = 0o700
	dataDirMode     = 0o700
	ownerSubdirList = ownerTracksDir + "," + ownerPacksDir + "," + ownerCacheDir + "," + ownerTmpDir
)

// ownerSpaces hands out rooted views of owner directories. Roots are opened
// per use and closed by the caller, so the set of owners is unbounded without
// holding a descriptor per owner.
type ownerSpaces struct {
	dir  string
	root *os.Root
}

func openOwnerSpaces(dataDir string) (*ownerSpaces, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory is required")
	}
	if err := os.MkdirAll(filepath.Join(dataDir, ownersDir), dataDirMode); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, fmt.Errorf("open data directory: %w", err)
	}
	return &ownerSpaces{dir: dataDir, root: root}, nil
}

func (o *ownerSpaces) close() error {
	if o == nil || o.root == nil {
		return nil
	}
	return o.root.Close()
}

// ownerPath is the path of an owner's directory relative to the data root.
func ownerPath(owner Owner) string {
	return filepath.Join(ownersDir, string(owner))
}

// open returns a root confined to the owner's directory, creating it on
// first use. Only valid owners have a directory: the key is the path.
func (o *ownerSpaces) open(owner Owner) (*os.Root, error) {
	if !owner.Valid() {
		return nil, errNoOwner
	}
	base := ownerPath(owner)
	for _, sub := range strings.Split(ownerSubdirList, ",") {
		if err := o.root.MkdirAll(filepath.Join(base, sub), ownerDirMode); err != nil {
			return nil, err
		}
	}
	// A pre-existing directory keeps whatever mode it was created with;
	// private data must not be group or world readable.
	if err := o.root.Chmod(base, ownerDirMode); err != nil {
		return nil, err
	}
	return o.root.OpenRoot(base)
}

// openSub returns a root confined to one subdirectory of the owner's space.
func (o *ownerSpaces) openSub(owner Owner, sub string) (*os.Root, error) {
	space, err := o.open(owner)
	if err != nil {
		return nil, err
	}
	defer space.Close()
	return space.OpenRoot(sub)
}

// exists reports whether the owner has a directory, without creating one.
func (o *ownerSpaces) exists(owner Owner) bool {
	if !owner.Valid() {
		return false
	}
	info, err := o.root.Lstat(ownerPath(owner))
	return err == nil && info.IsDir()
}

// list returns every owner with a directory. Names that are not valid owner
// keys are ignored: nothing in the server creates them.
func (o *ownerSpaces) list() ([]Owner, error) {
	entries, err := fs.ReadDir(o.root.FS(), ownersDir)
	if err != nil {
		return nil, err
	}
	owners := make([]Owner, 0, len(entries))
	for _, entry := range entries {
		owner := Owner(entry.Name())
		if entry.IsDir() && owner.Valid() {
			owners = append(owners, owner)
		}
	}
	return owners, nil
}

// remove deletes an owner's directory and everything in it.
func (o *ownerSpaces) remove(owner Owner) error {
	if !owner.Valid() {
		return errNoOwner
	}
	err := o.root.RemoveAll(ownerPath(owner))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// absolute returns the owner directory's path on disk, for free-space checks.
func (o *ownerSpaces) absolute(owner Owner) string {
	return filepath.Join(o.dir, ownerPath(owner))
}

// OpenOwnerTracks opens an owner's track directory under dataDir for the CLI,
// creating it with the same layout and modes the server uses. The caller
// closes the root.
func OpenOwnerTracks(dataDir string, owner Owner) (*os.Root, error) {
	spaces, err := openOwnerSpaces(dataDir)
	if err != nil {
		return nil, err
	}
	defer spaces.close()
	return spaces.openSub(owner, ownerTracksDir)
}

// RemoveOwner deletes everything an owner has under dataDir: tracks, pack
// manifests and owner-scoped cache entries. Shared cache objects their packs
// pinned stay pinned until the server next starts and finds no manifest
// referring to them; stop the server first, or restart it afterwards. It
// creates nothing: a data directory that does not exist has nothing to
// remove.
func RemoveOwner(dataDir string, owner Owner) error {
	if !owner.Valid() {
		return errNoOwner
	}
	root, err := os.OpenRoot(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.RemoveAll(ownerPath(owner))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
