package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func newTestServer(t *testing.T, assets ...fstest.MapFS) *Server {
	t.Helper()
	cfg := Config{
		GPXDir:           t.TempDir(),
		ElevationHost:    "http://elevation.invalid",
		ElevationDataset: "srtm30m",
	}
	if len(assets) == 1 {
		cfg.Assets = assets[0]
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func do(t *testing.T, s *Server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestSafeGPXPathRejectsEscapes(t *testing.T) {
	dir := "/library"
	bad := []string{
		"../../etc/passwd.gpx",
		"../secret.gpx",
		"sub/dir.gpx",
		`sub\dir.gpx`,
		"/etc/passwd.gpx",
		"notes.txt",
		"",
		"..",
		"track.gpx\x00.txt",
	}
	for _, name := range bad {
		if got, err := safeGPXPath(dir, name); err == nil {
			t.Errorf("safeGPXPath(%q) = %q, want error", name, got)
		}
	}

	for _, name := range []string{"track.gpx", "Mountain Route.GPX", "2020-01-01_09-30_Wed.gpx"} {
		got, err := safeGPXPath(dir, name)
		if err != nil {
			t.Errorf("safeGPXPath(%q): unexpected error %v", name, err)
			continue
		}
		if want := filepath.Join(dir, name); got != want {
			t.Errorf("safeGPXPath(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestTraversalDeleteIsRefused(t *testing.T) {
	s := newTestServer(t)
	victim := filepath.Join(filepath.Dir(s.gpxDir), "victim.gpx")
	if err := os.WriteFile(victim, []byte("<gpx/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	// %2F keeps the traversal in a single path segment, so it reaches the
	// handler as the literal name "../victim.gpx".
	rec := do(t, s, http.MethodDelete, "/gpx/..%2Fvictim.gpx", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("file outside the library was deleted: %v", err)
	}
}

func TestFileLifecycle(t *testing.T) {
	s := newTestServer(t)
	const content = `<?xml version="1.0"?><gpx version="1.1"><trk><name>Guara</name></trk></gpx>`

	if rec := do(t, s, http.MethodPut, "/gpx/guara.gpx", bytes.NewBufferString(content)); rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	rec := do(t, s, http.MethodGet, "/files", nil)
	var list struct{ Files []string }
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Files) != 1 || list.Files[0] != "guara.gpx" {
		t.Fatalf("files = %v, want [guara.gpx]", list.Files)
	}

	rec = do(t, s, http.MethodGet, "/gpx/guara.gpx", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != content {
		t.Fatalf("GET = %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	if rec := do(t, s, http.MethodDelete, "/gpx/guara.gpx", nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/gpx/guara.gpx", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete = %d, want 404", rec.Code)
	}
	if rec := do(t, s, http.MethodDelete, "/gpx/guara.gpx", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing = %d, want 404", rec.Code)
	}
}

func TestSaveRejectsEmptyBody(t *testing.T) {
	s := newTestServer(t)
	if rec := do(t, s, http.MethodPut, "/gpx/empty.gpx", bytes.NewReader(nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestUpload(t *testing.T) {
	s := newTestServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "morning-loop.gpx")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("<gpx/>"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	saved, err := os.ReadFile(filepath.Join(s.gpxDir, "morning-loop.gpx"))
	if err != nil || string(saved) != "<gpx/>" {
		t.Fatalf("saved = %q, err = %v", saved, err)
	}
}

func TestUploadRejectsNonGPX(t *testing.T) {
	s := newTestServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "../evil.sh")
	part.Write([]byte("rm -rf /"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestListingSkipsNonGPXAndTempFiles(t *testing.T) {
	s := newTestServer(t)
	for _, name := range []string{"a.gpx", "b.GPX", "notes.txt", ".tmp-123.gpx"} {
		if err := os.WriteFile(filepath.Join(s.gpxDir, name), []byte("<gpx/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(s.gpxDir, "nested.gpx"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := do(t, s, http.MethodGet, "/files", nil)
	var list struct{ Files []string }
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Files) != 2 || list.Files[0] != "a.gpx" || list.Files[1] != "b.GPX" {
		t.Fatalf("files = %v, want [a.gpx b.GPX]", list.Files)
	}
}

func TestCORS(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodOptions, "/gpx/track.gpx", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("missing Allow-Methods on preflight")
	}
	// Credentials must stay off while the origin is "*".
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials = %q, want unset", got)
	}
}

func TestServesEmbeddedFrontend(t *testing.T) {
	assets := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><div id=root>")},
		"assets/index-abc.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc.css": {Data: []byte("body{}")},
	}
	s := newTestServer(t, assets)

	rec := do(t, s, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("id=root")) {
		t.Fatalf("GET / = %d %q", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index Cache-Control = %q, want no-cache", cc)
	}

	rec = do(t, s, http.MethodGet, "/assets/index-abc.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("asset status = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q", cc)
	}

	// Unknown extensionless paths fall back to the SPA; missing assets 404.
	if rec := do(t, s, http.MethodGet, "/plan/new", nil); rec.Code != http.StatusOK {
		t.Errorf("SPA fallback = %d, want 200", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/assets/gone.js", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset = %d, want 404", rec.Code)
	}
}

func TestAPIRoutesWinOverFrontend(t *testing.T) {
	assets := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}
	s := newTestServer(t, assets)

	rec := do(t, s, http.MethodGet, "/files", nil)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("/files Content-Type = %q, want application/json", ct)
	}
}
