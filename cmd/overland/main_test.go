package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandLayout(t *testing.T) {
	cmd := newCommand()
	if cmd.Action == nil {
		t.Fatal("root command does not display help")
	}
	for _, name := range []string{"serve", "import", "user"} {
		if cmd.Command(name) == nil {
			t.Errorf("missing %q command", name)
		}
	}
}

func TestRootCommandDisplaysHelp(t *testing.T) {
	var output bytes.Buffer
	cmd := newCommand()
	cmd.Writer = &output
	if err := cmd.Run(context.Background(), []string{"overland"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "COMMANDS:") {
		t.Fatalf("root output did not contain command help:\n%s", output.String())
	}
}

func TestImportCommand(t *testing.T) {
	sourceDir := t.TempDir()
	sources := []string{
		filepath.Join(sourceDir, "route.gpx"),
		filepath.Join(sourceDir, "second.gpx"),
	}
	for _, source := range sources {
		if err := os.WriteFile(source, []byte("<gpx/>"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	library := t.TempDir()
	cmd := newCommand()
	cmd.Writer = io.Discard
	cmd.ErrWriter = io.Discard
	if err := cmd.Run(context.Background(), []string{
		"overland", "import", "--gpx-dir", library, sources[0], sources[1],
	}); err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		content, err := os.ReadFile(filepath.Join(library, filepath.Base(source)))
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "<gpx/>" {
			t.Fatalf("imported content = %q", content)
		}
	}
}

// naked starts services as "<binary> --port N <args>", so the root flag must
// reach serve. An out-of-range port fails before anything is started.
func TestRootPortFlagReachesServe(t *testing.T) {
	err := newCommand().Run(context.Background(), []string{"overland", "--port", "70000", "serve"})
	if err == nil || !strings.Contains(err.Error(), "--port 70000 is out of range") {
		t.Fatalf("err = %v", err)
	}
}
