package server

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// routePatterns lists every route the router serves, as "METHOD pattern".
func routePatterns(t *testing.T, s *Server) []string {
	t.Helper()
	router, ok := s.handler.(chi.Router)
	if !ok {
		t.Fatalf("handler is %T, not a chi router", s.handler)
	}
	var patterns []string
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		patterns = append(patterns, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return patterns
}

// concreteURL turns a chi pattern into a request target.
func concreteURL(pattern string) string {
	url := regexp.MustCompile(`\{filename\}`).ReplaceAllString(pattern, "trip.gpx")
	url = regexp.MustCompile(`\{id\}`).ReplaceAllString(url, strings.Repeat("a", 32))
	url = regexp.MustCompile(`\{variant:[^}]+\}`).ReplaceAllString(url, "sprite.json")
	url = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(url, "1")
	return strings.TrimSuffix(url, "*")
}

// Every route names whose data it serves, or the registry test fails: a new
// endpoint cannot quietly serve one owner's data to another.
func TestEveryRouteHasADataClass(t *testing.T) {
	s, err := New(Config{
		DataDir: t.TempDir(), OfflineCacheDir: t.TempDir(), RoutingCacheDir: t.TempDir(),
		ElevationTiles: true, ElevationTileCache: t.TempDir(), OpenFreeMapURL: "https://tiles.example.test/style.json",
		MCPBrowserHandler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	patterns := routePatterns(t, s)
	if len(patterns) < 40 {
		t.Fatalf("only %d routes walked", len(patterns))
	}
	classes := map[routeClass]int{}
	for _, pattern := range patterns {
		if strings.Contains(pattern, "/mcp/browser") {
			continue // mounted handler, guarded by its own capability cookie
		}
		class, ok := s.routeClasses[pattern]
		if !ok || class == 0 {
			t.Errorf("route %q has no data class", pattern)
		}
		classes[class]++
	}
	if classes[ownerRoute] < 10 || classes[operatorRoute] < 5 || classes[publicRoute] < 10 {
		t.Fatalf("class counts = %v", classes)
	}
}

// With RequireOwner every owner-scoped route refuses a request that carries
// no owner, before its handler runs; public routes still answer.
func TestOwnerRoutesFailClosedWithoutAnOwner(t *testing.T) {
	s, err := New(Config{
		DataDir: t.TempDir(), OfflineCacheDir: t.TempDir(), RoutingCacheDir: t.TempDir(), RequireOwner: true,
		ElevationTiles: true, ElevationTileCache: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	for pattern, class := range s.routeClasses {
		method, route, _ := strings.Cut(pattern, " ")
		if method == http.MethodOptions {
			continue
		}
		rec := do(t, s, method, concreteURL(route), nil)
		switch class {
		case ownerRoute, operatorRoute:
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("%s without an owner = %d, want 500", pattern, rec.Code)
			}
		case publicRoute:
			if rec.Code == http.StatusInternalServerError {
				t.Errorf("%s refused as if owner-scoped: %s", pattern, rec.Body)
			}
		}
	}
}

// Under sign-in, operator routes refuse a signed-in user who is not marked
// as an operator; with the mark they get past the guard.
func TestOperatorRoutesRefuseOrdinaryUsers(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), OfflineCacheDir: t.TempDir(), RoutingCacheDir: t.TempDir(), RequireOwner: true, TrustedUIOrigin: "http://localhost:8000"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	operators := 0
	for pattern, class := range s.routeClasses {
		if class != operatorRoute {
			continue
		}
		operators++
		method, route, _ := strings.Cut(pattern, " ")
		header := map[string]string{"X-GPX-Editor": "1", "Content-Type": "application/json", "Origin": "http://localhost:8000"}
		if rec := doAs(t, s, ownerA, method, concreteURL(route), strings.NewReader(`{}`), header); rec.Code != http.StatusForbidden {
			t.Errorf("%s for an ordinary user = %d, want 403", pattern, rec.Code)
		}
		req := httptest.NewRequest(method, concreteURL(route), strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:1"
		for k, v := range header {
			req.Header.Set(k, v)
		}
		req = asOperator(req.WithContext(WithOwner(req.Context(), ownerA)))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("%s refused the operator: %s", pattern, rec.Body)
		}
	}
	if operators < 5 {
		t.Fatalf("only %d operator routes", operators)
	}
}

func TestUploadConflictsAreConfinedToTheOwner(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), RequireOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	upload := func(owner Owner) *httptest.ResponseRecorder {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, _ := form.CreateFormFile("file", "trip.gpx")
		fmt.Fprint(part, "<gpx/>")
		form.Close()
		return doAs(t, s, owner, http.MethodPost, "/upload", &body, map[string]string{"Content-Type": form.FormDataContentType()})
	}
	if rec := upload(ownerA); rec.Code != http.StatusOK {
		t.Fatalf("A upload = %d %s", rec.Code, rec.Body)
	}
	if rec := upload(ownerA); rec.Code != http.StatusConflict {
		t.Fatalf("A second upload = %d", rec.Code)
	}
	// B's upload is not told that A has a trip.gpx.
	if rec := upload(ownerB); rec.Code != http.StatusOK {
		t.Fatalf("B upload = %d %s", rec.Code, rec.Body)
	}
	for _, owner := range []Owner{ownerA, ownerB} {
		if _, err := os.Stat(filepath.Join(s.dataDir, ownersDir, string(owner), ownerTracksDir, "trip.gpx")); err != nil {
			t.Fatalf("owner %s track: %v", owner[:4], err)
		}
	}
	if rec := doAs(t, s, ownerA, http.MethodGet, "/files", nil, nil); strings.Count(rec.Body.String(), "trip.gpx") != 1 {
		t.Fatalf("A files = %s", rec.Body)
	}
}

func TestTripPacksBelongToTheirOwner(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Fecha":"today","ListaEESSPrecio":[]}`)
	}))
	defer upstream.Close()
	s, err := New(Config{DataDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(), FuelURL: upstream.URL, RequireOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	input := packInput{Name: "Andalucía trip", BBox: &bbox{South: 36, West: -6, North: 38, East: -4}, Scopes: []string{"fuel"}}
	mine, _, err := s.packs.start(ownerA, input)
	if err != nil {
		t.Fatal(err)
	}
	waitForOwnerPackState(t, s.packs, ownerA, mine.ID, "complete")

	header := map[string]string{"X-GPX-Editor": "1"}
	if rec := doAs(t, s, ownerB, http.MethodGet, "/offline/packs", nil, nil); strings.Contains(rec.Body.String(), mine.ID) || strings.Contains(rec.Body.String(), "Andalucía") {
		t.Fatalf("B lists A's pack: %s", rec.Body)
	}
	if rec := doAs(t, s, ownerB, http.MethodGet, "/offline/packs/"+mine.ID, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("B read A's pack = %d", rec.Code)
	}
	if rec := doAs(t, s, ownerB, http.MethodPost, "/offline/packs/"+mine.ID+"/cancel", nil, header); rec.Code != http.StatusNotFound {
		t.Fatalf("B cancelled A's pack = %d", rec.Code)
	}
	if rec := doAs(t, s, ownerB, http.MethodDelete, "/offline/packs/"+mine.ID, nil, header); rec.Code != http.StatusNotFound {
		t.Fatalf("B deleted A's pack = %d", rec.Code)
	}
	if rec := doAs(t, s, ownerB, http.MethodGet, "/offline/status", nil, nil); !strings.Contains(rec.Body.String(), `"activeJobs":0`) {
		t.Fatalf("B's status counts A's job: %s", rec.Body)
	}
	if rec := doAs(t, s, ownerA, http.MethodGet, "/offline/packs/"+mine.ID, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("A lost its pack: %d", rec.Code)
	}
	// The manifest is in A's directory and nowhere else.
	if _, err := os.Stat(filepath.Join(s.dataDir, ownersDir, string(ownerA), ownerPacksDir, mine.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.cache.dir, "v1", "packs")); !os.IsNotExist(err) {
		t.Fatalf("manifests still in the shared cache: %v", err)
	}

	// B downloading the same area gets a pack of their own that shares the
	// cached object; deleting A's pack must not take it from B.
	theirs, _, err := s.packs.start(ownerB, input)
	if err != nil {
		t.Fatal(err)
	}
	if theirs.ID == mine.ID {
		t.Fatal("B was handed A's pack")
	}
	waitForOwnerPackState(t, s.packs, ownerB, theirs.ID, "complete")
	keys := s.cache.keysForScope("fuel")
	if len(keys) != 1 {
		t.Fatalf("fuel keys = %v", keys)
	}
	pinsOf := func() []string {
		s.cache.mu.Lock()
		defer s.cache.mu.Unlock()
		return append([]string(nil), s.cache.entries[keys[0]].Pins...)
	}
	if pins := pinsOf(); len(pins) != 2 {
		t.Fatalf("pins = %v, want both packs", pins)
	}
	if rec := doAs(t, s, ownerA, http.MethodDelete, "/offline/packs/"+mine.ID, nil, header); rec.Code != http.StatusNoContent {
		t.Fatalf("A delete = %d %s", rec.Code, rec.Body)
	}
	if pins := pinsOf(); len(pins) != 1 || pins[0] != theirs.ID {
		t.Fatalf("pins after A's delete = %v, want B's only", pins)
	}
	if summary, ok := s.packs.publicManifest(ownerB, theirs.ID); !ok || summary.State != "complete" {
		t.Fatalf("B's pack after A's delete = %+v", summary)
	}
	// Owner directories are private, and nothing under the shared cache is
	// owner-scoped.
	for _, owner := range []Owner{ownerA, ownerB} {
		info, err := os.Stat(filepath.Join(s.dataDir, ownersDir, string(owner)))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("owner dir mode = %v, %v", info.Mode(), err)
		}
	}
	for scope := range s.cache.stats().Scopes {
		if s.ownerScopes[scope] {
			t.Fatalf("shared cache holds owner scope %q", scope)
		}
	}
}

func waitForOwnerPackState(t *testing.T, packs *packManager, owner Owner, id, state string) packSummary {
	t.Helper()
	for range 3000 {
		summary, ok := packs.publicManifest(owner, id)
		if !ok {
			t.Fatal("pack disappeared")
		}
		if summary.State == state {
			return summary
		}
		if summary.State != "queued" && summary.State != "running" {
			t.Fatalf("pack reached %q, want %q: %+v", summary.State, state, summary)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pack did not reach %q", state)
	return packSummary{}
}

// A signed-in owner's share of the pack queue is bounded; the local owner,
// alone on its server, keeps the whole queue.
func TestOwnersHaveABoundedShareOfThePackQueue(t *testing.T) {
	s := fuelPackServer(t)
	occupySlots(s)
	var first string
	for i := range maxOwnerPackJobs {
		input := packInput{Name: fmt.Sprintf("part %d", i), Regional: true, BBox: &bbox{South: 40 + float64(i)*0.1, West: -1, North: 40.05 + float64(i)*0.1, East: 0}, Scopes: []string{"fuel"}}
		pack, _, err := s.packs.start(ownerA, input)
		if err != nil {
			t.Fatalf("pack %d: %v", i, err)
		}
		if i == 0 {
			first = pack.ID
		}
	}
	over := packInput{Name: "one too many", Regional: true, BBox: &bbox{South: 43, West: -8, North: 43.05, East: -7}, Scopes: []string{"fuel"}}
	if _, _, err := s.packs.start(ownerA, over); err == nil || !strings.Contains(err.Error(), "your pack jobs") {
		t.Fatalf("over the owner's share = %v", err)
	}
	if _, _, err := s.packs.start(ownerB, over); err != nil {
		t.Fatalf("another owner was refused by A's share: %v", err)
	}
	// Both wait behind the same slots; A queued first, so A goes first.
	s.packs.mu.Lock()
	next := s.packs.nextQueuedLocked()
	if next == nil || next.owner != ownerA {
		s.packs.mu.Unlock()
		t.Fatalf("next queued pack belongs to %v, want A", next)
	}
	// Once A has a pack running, the next slot goes to the owner with none.
	s.packs.packs[first].State = "running"
	next = s.packs.nextQueuedLocked()
	s.packs.mu.Unlock()
	if next == nil || next.owner != ownerB {
		t.Fatalf("next queued pack belongs to %v, want B", next)
	}
}

func TestRemoveOwnerCreatesNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	if err := RemoveOwner(missing, ownerA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("RemoveOwner created the data directory: %v", err)
	}
	if err := RemoveOwner(missing, Owner("../etc")); err == nil {
		t.Fatal("an invalid owner was accepted")
	}
	dataDir := t.TempDir()
	root, err := OpenOwnerTracks(dataDir, ownerA)
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err := RemoveOwner(dataDir, ownerA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ownersDir, string(ownerA))); !os.IsNotExist(err) {
		t.Fatalf("owner directory remains: %v", err)
	}
}
