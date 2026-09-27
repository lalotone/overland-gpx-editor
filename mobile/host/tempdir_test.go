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
	stale := filepath.Join(dir, "overland-session-profile-1")
	if err := os.WriteFile(stale, []byte("scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UsePrivateTempDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temporary file survived: %v", err)
	}
	if got := os.TempDir(); got != dir {
		t.Fatalf("os.TempDir() = %q, want %q", got, dir)
	}
	created, err := os.MkdirTemp("", "overland-session-profile-*")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(created) != dir {
		t.Fatalf("temporary directory created in %q, want %q", filepath.Dir(created), dir)
	}
}
