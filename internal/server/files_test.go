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
	"strings"
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
	cleanupTestServer(t, s)
	return s
}

func cleanupTestServer(t *testing.T, s *Server) {
	t.Helper()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

func do(t *testing.T, s *Server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestConfigEndpoint(t *testing.T) {
	tests := []struct {
		name string
		set  string
		want string
	}{
		{name: "default", want: defaultNominatimURL},
		{name: "configured", set: " https://search.example.test/ ", want: "https://search.example.test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(Config{
				GPXDir:        t.TempDir(),
				ElevationHost: "http://elevation.invalid",
				NominatimURL:  tt.set,
			})
			if err != nil {
				t.Fatal(err)
			}
			cleanupTestServer(t, s)

			rec := do(t, s, http.MethodGet, "/config", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var got struct {
				NominatimURL string `json:"nominatimUrl"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.NominatimURL != tt.want {
				t.Errorf("nominatimUrl = %q, want %q", got.NominatimURL, tt.want)
			}
			if cache := rec.Header().Get("Cache-Control"); cache != "no-cache" {
				t.Errorf("Cache-Control = %q, want no-cache", cache)
			}
		})
	}
}

func TestSafeGPXFilenameRejectsEscapes(t *testing.T) {
	bad := []string{
		"../../etc/passwd.gpx",
		"../secret.gpx",
		"sub/dir.gpx",
		`sub\dir.gpx`,
		"/etc/passwd.gpx",
		"notes.txt",
		".hidden.gpx",
		"",
		"..",
		"track.gpx\x00.txt",
	}
	for _, name := range bad {
		if got, err := safeGPXFilename(name); err == nil {
			t.Errorf("safeGPXFilename(%q) = %q, want error", name, got)
		}
	}

	for _, name := range []string{"track.gpx", "Mountain Route.GPX", "2020-01-01_09-30_Wed.gpx"} {
		got, err := safeGPXFilename(name)
		if err != nil {
			t.Errorf("safeGPXFilename(%q): unexpected error %v", name, err)
			continue
		}
		if got != name {
			t.Errorf("safeGPXFilename(%q) = %q", name, got)
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

func TestGPXRootRejectsSymlinkEscape(t *testing.T) {
	s := newTestServer(t)
	outside := filepath.Join(t.TempDir(), "outside.gpx")
	const secret = "outside the GPX directory"
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.gpxDir, "leak.gpx")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rec := do(t, s, http.MethodGet, "/gpx/leak.gpx", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET symlink status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(secret)) {
		t.Fatal("GET disclosed a file outside the GPX library")
	}

	rec = do(t, s, http.MethodGet, "/files", nil)
	if bytes.Contains(rec.Body.Bytes(), []byte("leak.gpx")) {
		t.Fatal("library listing included an escaping symlink")
	}
}

func TestSaveReplacesSymlinkWithoutFollowingIt(t *testing.T) {
	s := newTestServer(t)
	outside := filepath.Join(t.TempDir(), "outside.gpx")
	const original = "<gpx><name>outside</name></gpx>"
	const replacement = "<gpx><name>inside</name></gpx>"
	if err := os.WriteFile(outside, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(s.gpxDir, "route.gpx")
	if err := os.Symlink(outside, inside); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rec := do(t, s, http.MethodPost, "/gpx/route.gpx", bytes.NewBufferString(replacement))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	gotOutside, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotOutside) != original {
		t.Fatalf("save followed symlink and replaced outside file: %q", gotOutside)
	}
	gotInside, err := os.ReadFile(inside)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotInside) != replacement {
		t.Fatalf("saved file = %q, want %q", gotInside, replacement)
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

func newUploadRequest(t *testing.T, filename, content string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func uploadFile(t *testing.T, s *Server, filename, content string) *httptest.ResponseRecorder {
	t.Helper()
	req := newUploadRequest(t, filename, content)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestUpload(t *testing.T) {
	s := newTestServer(t)
	rec := uploadFile(t, s, "morning-loop.gpx", "<gpx/>")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	saved, err := os.ReadFile(filepath.Join(s.gpxDir, "morning-loop.gpx"))
	if err != nil || string(saved) != "<gpx/>" {
		t.Fatalf("saved = %q, err = %v", saved, err)
	}
}

func TestUploadRejectsOversizedBody(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/upload", nil)
	req.ContentLength = maxUploadBytes + 1
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", rec.Code, rec.Body)
	}
}

func TestUploadDoesNotFollowSymlink(t *testing.T) {
	s := newTestServer(t)
	outside := filepath.Join(t.TempDir(), "outside.gpx")
	const original = "<gpx><name>outside</name></gpx>"
	if err := os.WriteFile(outside, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.gpxDir, "route.gpx")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rec := uploadFile(t, s, "route.gpx", "<gpx><name>replacement</name></gpx>")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != original {
		t.Fatalf("upload followed symlink and replaced outside file: %q", content)
	}
}

func TestUploadDoesNotReplaceExistingFile(t *testing.T) {
	s := newTestServer(t)
	path := filepath.Join(s.gpxDir, "route.gpx")
	const original = "<gpx><metadata><name>original</name></metadata></gpx>"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := uploadFile(t, s, "route.gpx", "<gpx><metadata><name>replacement</name></metadata></gpx>")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != original {
		t.Fatalf("existing file was replaced: %q", saved)
	}
}

func TestConcurrentUploadsDoNotReplace(t *testing.T) {
	s := newTestServer(t)
	requests := []*http.Request{
		newUploadRequest(t, "route.gpx", "<gpx><name>first</name></gpx>"),
		newUploadRequest(t, "route.gpx", "<gpx><name>second</name></gpx>"),
	}
	start := make(chan struct{})
	results := make(chan int, 2)
	for _, req := range requests {
		go func(req *http.Request) {
			<-start
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			results <- rec.Code
		}(req)
	}
	close(start)

	succeeded, conflicted := 0, 0
	for range 2 {
		switch status := <-results; status {
		case http.StatusOK:
			succeeded++
		case http.StatusConflict:
			conflicted++
		default:
			t.Fatalf("upload status = %d, want 200 or 409", status)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded = %d, conflicted = %d; want 1 each", succeeded, conflicted)
	}
	content, err := os.ReadFile(filepath.Join(s.gpxDir, "route.gpx"))
	if err != nil {
		t.Fatal(err)
	}
	first := "<gpx><name>first</name></gpx>"
	second := "<gpx><name>second</name></gpx>"
	if string(content) != first && string(content) != second {
		t.Fatalf("created partial or unexpected content: %q", content)
	}
}

func TestUploadRejectsNonGPX(t *testing.T) {
	s := newTestServer(t)
	rec := uploadFile(t, s, "../evil.sh", "rm -rf /")

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
	s, err := New(Config{
		GPXDir:           t.TempDir(),
		ElevationHost:    "http://elevation.invalid",
		ElevationDataset: "srtm30m",
		AllowedOrigins:   []string{"https://planner.example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)

	req := httptest.NewRequest(http.MethodOptions, "/gpx/track.gpx", nil)
	req.Header.Set("Origin", "https://planner.example.test")
	req.Header.Set("Access-Control-Request-Method", http.MethodPut)
	req.Header.Set("Access-Control-Request-Headers", "Content-Type")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://planner.example.test" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != http.MethodPut {
		t.Errorf("Allow-Methods = %q, want PUT", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials = %q, want unset", got)
	}

	req = httptest.NewRequest(http.MethodOptions, "/gpx/track.gpx", nil)
	req.Header.Set("Origin", "https://evil.example.test")
	req.Header.Set("Access-Control-Request-Method", http.MethodPut)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("denied Allow-Origin = %q, want unset", got)
	}
}

func TestCrossOriginWritesAreRefused(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/gpx/track.gpx", bytes.NewBufferString("<gpx/>"))
	req.Header.Set("Origin", "https://evil.example.test")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(s.gpxDir, "track.gpx")); !os.IsNotExist(err) {
		t.Fatalf("cross-origin write created a file: %v", err)
	}

	req = httptest.NewRequest(http.MethodPut, "https://public.example.test/gpx/public.gpx", bytes.NewBufferString("<gpx/>"))
	req.Header.Set("Origin", "https://public.example.test")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unconfigured public origin status = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/gpx/fetch-metadata.gpx", bytes.NewBufferString("<gpx/>"))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site fetch status = %d, want 403", rec.Code)
	}
}

func TestConfiguredOriginCanWrite(t *testing.T) {
	s, err := New(Config{
		GPXDir:         t.TempDir(),
		ElevationHost:  "http://elevation.invalid",
		AllowedOrigins: []string{"https://planner.example.test/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)

	req := httptest.NewRequest(http.MethodPut, "/gpx/track.gpx", bytes.NewBufferString("<gpx/>"))
	req.Header.Set("Origin", "https://planner.example.test")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://planner.example.test" {
		t.Errorf("Allow-Origin = %q", got)
	}
}

func TestLoopbackSameOriginCanWrite(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:8000/gpx/track.gpx", bytes.NewBufferString("<gpx/>"))
	req.Header.Set("Origin", "http://127.0.0.1:8000")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

func TestInvalidAllowedOriginIsRejected(t *testing.T) {
	_, err := New(Config{
		GPXDir:         t.TempDir(),
		AllowedOrigins: []string{"https://example.test/path"},
	})
	if err == nil {
		t.Fatal("New accepted an origin containing a path")
	}
}

func TestHTTPHardening(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("health status = %d, want 204", rec.Code)
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	rec = do(t, s, http.MethodPost, "/files", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /files status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow == "" {
		t.Error("POST /files response has no Allow header")
	}

	rec = do(t, s, http.MethodHead, "/files", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD /files status = %d, want 200", rec.Code)
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
	if !strings.Contains(rec.Body.String(), `name="gpx-editor-offline-mode" content="auto"`) {
		t.Fatalf("index did not bootstrap auto policy: %s", rec.Body)
	}
	if _, err := s.modes.set(modeCacheOnly); err != nil {
		t.Fatal(err)
	}
	rec = do(t, s, http.MethodGet, "/", nil)
	if !strings.Contains(rec.Body.String(), `name="gpx-editor-offline-mode" content="cache-only"`) {
		t.Fatalf("index did not bootstrap cache-only policy: %s", rec.Body)
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
