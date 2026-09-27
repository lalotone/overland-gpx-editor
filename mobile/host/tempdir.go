package host

import "os"

// UsePrivateTempDir points TMPDIR at dir, emptied first. Without TMPDIR,
// os.TempDir on Android is /data/local/tmp, which apps cannot write, so every
// temporary file the backend creates would fail: the metric directory of a
// session-uploaded BRF profile, and multipart uploads too large to buffer in
// memory. Nothing in dir is meant to outlive the process, and a killed process
// cannot clean up after itself, so it starts empty.
func UsePrivateTempDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.Setenv("TMPDIR", dir)
}
