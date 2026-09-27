package host

import "os"

// UsePrivateTempDir points TMPDIR at dir, emptied first. Without TMPDIR,
// os.TempDir on Android is /data/local/tmp, which apps cannot write, and Broom
// inflates each gzip DEM tile into a ~25 MB scratch file there while building
// a routing graph with elevation. Scratch files are removed when the build
// closes its tiles, but a killed process leaves them behind, so nothing in dir
// survives a restart.
func UsePrivateTempDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.Setenv("TMPDIR", dir)
}
