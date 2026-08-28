package importcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportFile(t *testing.T) {
	sourceDir := t.TempDir()
	libraryDir := t.TempDir()
	source := filepath.Join(sourceDir, "route.GPX")
	const content = `<gpx version="1.1"><trk><name>Route</name></trk></gpx>`
	if err := os.WriteFile(source, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	filename, err := importFile(root, source)
	if err != nil {
		t.Fatal(err)
	}
	if filename != "route.GPX" {
		t.Fatalf("filename = %q, want route.GPX", filename)
	}
	got, err := os.ReadFile(filepath.Join(libraryDir, filename))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("imported content = %q", got)
	}
	entries, err := os.ReadDir(libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filename {
		t.Fatalf("library entries = %v, want only %s", entries, filename)
	}
}

func TestImportFileDoesNotReplaceExistingTrack(t *testing.T) {
	sourceDir := t.TempDir()
	libraryDir := t.TempDir()
	source := filepath.Join(sourceDir, "route.gpx")
	destination := filepath.Join(libraryDir, "route.gpx")
	if err := os.WriteFile(source, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if _, err := importFile(root, source); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("importFile error = %v, want already exists", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("existing track was replaced: %q", got)
	}
}

func TestImportFileRejectsNonGPX(t *testing.T) {
	source := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(source, []byte("not GPX"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := importFile(root, source); err == nil {
		t.Fatal("importFile accepted a non-GPX file")
	}
}

func TestImportFileRejectsHiddenName(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), ".hidden.gpx")
	if err := os.WriteFile(sourcePath, []byte("<gpx/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	libraryDir := t.TempDir()
	root, err := os.OpenRoot(libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if _, err := importFile(root, sourcePath); err == nil {
		t.Fatal("importFile accepted a hidden filename")
	}
}
