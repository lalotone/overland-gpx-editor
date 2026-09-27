package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUsePrivateTempDirClearsLeftoversAndRedirectsTempDir(t *testing.T) {
	t.Setenv("TMPDIR", "")
	dir := filepath.Join(t.TempDir(), "tmp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "broom-hgt-1.hgt")
	if err := os.WriteFile(stale, []byte("scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UsePrivateTempDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale scratch file survived: %v", err)
	}
	if got := os.TempDir(); got != dir {
		t.Fatalf("os.TempDir() = %q, want %q", got, dir)
	}
	scratch, err := os.CreateTemp("", "broom-hgt-*.hgt")
	if err != nil {
		t.Fatal(err)
	}
	scratch.Close()
	if filepath.Dir(scratch.Name()) != dir {
		t.Fatalf("scratch file created in %q, want %q", filepath.Dir(scratch.Name()), dir)
	}
}
