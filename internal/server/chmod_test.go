package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestChmodInStaysInsideRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := chmodIn(root, "cache", 0o700); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "cache")); info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %o, want 700", info.Mode().Perm())
	}
	if err := chmodIn(root, "escape", 0o700); err == nil {
		t.Error("chmodIn followed a symlink out of the root")
	}
}

// os.Root.Chmod needs fchmodat2, which Android's seccomp policy can answer by
// killing the app (SIGSYS) instead of an error Go could fall back from. It
// crashed the app at launch on Android 16; use chmodIn instead.
func TestNoRootChmodInServer(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`(?i)root\.Chmod\(`)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if call.MatchString(line) && !strings.Contains(line, "//") {
				t.Errorf("%s:%d calls os.Root.Chmod; use chmodIn: %s", file, i+1, strings.TrimSpace(line))
			}
		}
	}
}
