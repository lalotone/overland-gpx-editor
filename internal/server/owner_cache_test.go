package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// doAs sends a request on behalf of an owner, the way the passkey wrapper
// would, from a loopback browser on the trusted UI.
func doAs(t *testing.T, s *Server, owner Owner, method, target string, body io.Reader, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req = req.WithContext(WithOwner(req.Context(), owner))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// asOperator adds the operator mark the wrapper gives --auth-operator accounts.
func asOperator(req *http.Request) *http.Request {
	return req.WithContext(WithOperator(req.Context()))
}

// testOwner returns a distinct valid owner key for i.
func testOwner(i int) Owner {
	const alphabet = "abcdefgh"
	key := make([]byte, ownerKeyLength)
	for j := range key {
		key[j] = alphabet[i%len(alphabet)]
		i /= len(alphabet)
	}
	return Owner(key)
}

const (
	ownerA = Owner("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	ownerB = Owner("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

func placesServer(t *testing.T, calls *atomic.Int64) (*Server, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"place_id":1,"display_name":%q,"lat":"1","lon":"2"}]`, r.URL.Query().Get("q"))
	}))
	t.Cleanup(upstream.Close)
	s, err := New(Config{
		DataDir: t.TempDir(), ElevationHost: "http://elevation.invalid", OfflineCacheDir: t.TempDir(),
		NominatimURL: upstream.URL, RequireOwner: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	return s, upstream.URL
}

func TestEveryProviderPolicyIsClassified(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), OfflineCacheDir: t.TempDir(), OpenFreeMapURL: "https://tiles.example.test/style.json"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	policies := s.providerPolicies()
	if len(policies) < 6 {
		t.Fatalf("only %d policies listed", len(policies))
	}
	for _, policy := range policies {
		if policy.data != sharedProviderData && policy.data != ownerProviderData {
			t.Errorf("provider %s has no data classification", policy.name)
		}
	}
	if !s.ownerScopes["places"] || s.ownerScopes["fuel"] || s.ownerScopes["pois"] {
		t.Fatalf("owner scopes = %v", s.ownerScopes)
	}
	// An unclassified policy is refused rather than cached somewhere.
	unclassified := *s.providers["fuel"]
	unclassified.data = 0
	if _, err := s.outbound.do(WithOwner(t.Context(), LocalOwner), cachedRequest{policy: &unclassified, url: unclassified.baseURL.String(), params: "x", cacheable: true}); err == nil || !strings.Contains(err.Error(), "classification") {
		t.Fatalf("unclassified provider request = %v", err)
	}
}

func TestPlaceSearchesAreCachedPerOwner(t *testing.T) {
	var calls atomic.Int64
	s, _ := placesServer(t, &calls)
	const query = "/places/search?q=Calle+Mayor+12&language=es"
	first := doAs(t, s, ownerA, http.MethodGet, query, nil, nil)
	if first.Code != http.StatusOK || first.Header().Get("X-GPX-Cache") != "miss" {
		t.Fatalf("A first = %d cache=%q", first.Code, first.Header().Get("X-GPX-Cache"))
	}
	again := doAs(t, s, ownerA, http.MethodGet, query, nil, nil)
	if again.Header().Get("X-GPX-Cache") != "hit" {
		t.Fatalf("A again cache=%q", again.Header().Get("X-GPX-Cache"))
	}
	// B asking the same question learns nothing from A's history: no hit,
	// no cached-at date, and the provider is asked again.
	other := doAs(t, s, ownerB, http.MethodGet, query, nil, nil)
	if other.Code != http.StatusOK || other.Header().Get("X-GPX-Cache") != "miss" {
		t.Fatalf("B = %d cache=%q", other.Code, other.Header().Get("X-GPX-Cache"))
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want one per owner", calls.Load())
	}
	// The entries live with their owners, not in the shared cache.
	if stats := s.cache.stats(); stats.Entries != 0 {
		t.Fatalf("shared cache holds %d owner entries", stats.Entries)
	}
	for _, owner := range []Owner{ownerA, ownerB} {
		entries, err := os.ReadDir(filepath.Join(s.dataDir, ownersDir, string(owner), ownerCacheDir, "v1", "entries", "places"))
		if err != nil || len(entries) == 0 {
			t.Fatalf("owner %s has no cache entries: %v", owner[:4], err)
		}
	}
	// Status shows the caller their own scope.
	status := doAs(t, s, ownerA, http.MethodGet, "/offline/status", nil, nil)
	if !strings.Contains(status.Body.String(), `"places":{"bytes"`) {
		t.Fatalf("status lacks the owner's places scope: %s", status.Body)
	}
}

func TestOwnerScopedRequestsWithoutAnOwnerAreServedUncached(t *testing.T) {
	var calls atomic.Int64
	s, _ := placesServer(t, &calls)
	// A public-route caller has no owner under RequireOwner; the place route
	// itself refuses it. The outbound client, asked directly, bypasses.
	if rec := do(t, s, http.MethodGet, "/places/search?q=x", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("ownerless place search = %d", rec.Code)
	}
	p := s.providers["places"]
	response, err := s.outbound.do(t.Context(), cachedRequest{policy: p, url: p.baseURL.String() + "/search?q=x", params: "x", cacheable: true})
	if err != nil || response.State != "bypass" {
		t.Fatalf("ownerless outbound = %q, %v", response.State, err)
	}
}

func TestClearingCacheScopesRespectsOwnership(t *testing.T) {
	var calls atomic.Int64
	s, _ := placesServer(t, &calls)
	header := map[string]string{"X-GPX-Editor": "1"}
	for _, owner := range []Owner{ownerA, ownerB} {
		doAs(t, s, owner, http.MethodGet, "/places/search?q=home", nil, nil)
	}
	// B clears places: only B's entries go.
	if rec := doAs(t, s, ownerB, http.MethodDelete, "/offline/cache?scope=places", nil, header); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":1`) {
		t.Fatalf("B clear = %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, s, ownerA, http.MethodGet, "/places/search?q=home", nil, nil); rec.Header().Get("X-GPX-Cache") != "hit" {
		t.Fatalf("A lost its cache to B's clear: %q", rec.Header().Get("X-GPX-Cache"))
	}
	// Shared scopes, and everything, are the operator's.
	if rec := doAs(t, s, ownerA, http.MethodDelete, "/offline/cache?scope=fuel", nil, header); rec.Code != http.StatusForbidden {
		t.Fatalf("A cleared a shared scope: %d", rec.Code)
	}
	if rec := doAs(t, s, ownerA, http.MethodDelete, "/offline/cache", nil, header); rec.Code != http.StatusForbidden {
		t.Fatalf("A cleared everything: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodDelete, "/offline/cache", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("X-GPX-Editor", "1")
	req = asOperator(req.WithContext(WithOwner(req.Context(), ownerA)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":1`) {
		t.Fatalf("operator clear = %d %s", rec.Code, rec.Body)
	}
}

func TestOwnerCachesAreBoundedAndClosed(t *testing.T) {
	spaces, err := openOwnerSpaces(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer spaces.close()
	caches := newOwnerCaches(spaces, true, 1<<20, 16)
	for i := range maxOpenOwnerCaches + 3 {
		if caches.get(testOwner(i)) == nil {
			t.Fatalf("owner %d has no cache", i)
		}
	}
	if got := caches.aggregate().Open; got != maxOpenOwnerCaches {
		t.Fatalf("open stores = %d, want %d", got, maxOpenOwnerCaches)
	}
	if err := caches.close(); err != nil {
		t.Fatal(err)
	}
	if caches.aggregate().Open != 0 {
		t.Fatal("stores still open after close")
	}
	disabled := newOwnerCaches(spaces, false, 0, 0)
	if disabled.get(ownerA) != nil {
		t.Fatal("disabled caches opened a store")
	}
}
