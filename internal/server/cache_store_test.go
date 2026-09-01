package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testMetadata(key, scope string, now time.Time) cacheMetadata {
	return cacheMetadata{
		Key: key, Scope: scope, Provider: "test", SourceFingerprint: "source",
		Status: 200, Headers: map[string]string{}, ContentType: "application/json",
		FetchedAt: now, FreshUntil: now.Add(time.Hour), StaleUntil: now.Add(2 * time.Hour), LastAccess: now,
	}
}

func TestCanonicalCacheKeySeparatesRelevantInputs(t *testing.T) {
	base := canonicalCacheKey("provider", "source", "GET", "a=1", "en", []byte(`{"x":1}`))
	variants := []string{
		canonicalCacheKey("other", "source", "GET", "a=1", "en", []byte(`{"x":1}`)),
		canonicalCacheKey("provider", "other", "GET", "a=1", "en", []byte(`{"x":1}`)),
		canonicalCacheKey("provider", "source", "POST", "a=1", "en", []byte(`{"x":1}`)),
		canonicalCacheKey("provider", "source", "GET", "a=2", "en", []byte(`{"x":1}`)),
		canonicalCacheKey("provider", "source", "GET", "a=1", "es", []byte(`{"x":1}`)),
		canonicalCacheKey("provider", "source", "GET", "a=1", "en", []byte(`{"x":2}`)),
	}
	for i, variant := range variants {
		if variant == base {
			t.Errorf("variant %d did not alter the key", i)
		}
		if len(variant) != 64 {
			t.Errorf("variant %d has non-SHA256 key %q", i, variant)
		}
	}
}

func TestCacheStoreRejectsControlReservationAboveQuota(t *testing.T) {
	store, err := newCacheStore(t.TempDir(), 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setReservedBytes(1025); err == nil {
		t.Fatal("control reservation above the cache quota was accepted")
	}
}

func TestDisabledCacheIgnoresControlReservation(t *testing.T) {
	store, err := newCacheStore("", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setReservedBytes(1 << 20); err != nil {
		t.Fatalf("disabled cache rejected control reservation: %v", err)
	}
}

func TestCacheStorePersistsSecurelyAndRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	store, err := newCacheStore(dir, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	key := canonicalCacheKey("p", "s", "GET", "private query and 42.1,-0.2", "", nil)
	body := []byte(`{"ok":true}`)
	if err := store.put(testMetadata(key, "places", now), body); err != nil {
		t.Fatal(err)
	}
	bodyPath, metaPath, _ := store.paths("places", key)
	for _, path := range []string{bodyPath, metaPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o", filepath.Base(path), info.Mode().Perm())
		}
		if filepath.Base(path) != key+filepath.Ext(path) {
			t.Errorf("private input leaked into %q", path)
		}
	}

	restarted, err := newCacheStore(dir, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := restarted.get("places", key)
	if !ok || string(entry.Body) != string(body) {
		t.Fatalf("restart read = %q, %v", entry.Body, ok)
	}
	if err := os.WriteFile(bodyPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.get("places", key); ok {
		t.Fatal("corrupt body was served")
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Errorf("corrupt sidecar was not removed: %v", err)
	}
}

func TestCacheStoreRecoversInterruptedReplacement(t *testing.T) {
	dir := t.TempDir()
	store, err := newCacheStore(dir, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	key := canonicalCacheKey("test", "source", http.MethodGet, "replace", "", nil)
	if err := store.put(testMetadata(key, "places", time.Now().UTC()), []byte("old body")); err != nil {
		t.Fatal(err)
	}
	bodyPath, metaPath, err := store.paths("places", key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bodyPath, bodyPath+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(metaPath, metaPath+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bodyPath, []byte("partially committed new body"), 0o600); err != nil {
		t.Fatal(err)
	}

	restarted, err := newCacheStore(dir, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := restarted.get("places", key)
	if !ok || string(entry.Body) != "old body" {
		t.Fatalf("recovered entry = %q, %v", entry.Body, ok)
	}
	if _, err := os.Lstat(bodyPath + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("body backup remains: %v", err)
	}
}

func TestCacheStoreCleansOrphansAndTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := newCacheStore(dir, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	orphanDir := filepath.Join(store.entriesDir, "fuel", "aa")
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(orphanDir, stringsRepeat("a", 64)+".body")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(store.tmpDir, ".tmp-leftover")
	if err := os.WriteFile(temporary, []byte("tmp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newCacheStore(dir, 1<<20, 100); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{orphan, temporary} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s was not cleaned: %v", path, err)
		}
	}
}

func TestCacheStoreIntermediateSymlinkCannotEscapeRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	store, err := newCacheStore(dir, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	scopeDir := filepath.Join(store.entriesDir, "places")
	if err := os.Symlink(outside, scopeDir); err != nil {
		t.Fatal(err)
	}
	key := canonicalCacheKey("p", "s", http.MethodGet, "escape", "", nil)
	if err := store.put(testMetadata(key, "places", time.Now().UTC()), []byte("private")); err == nil {
		t.Fatal("cache write followed an escaping intermediate symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache wrote outside its root: %v", entries)
	}
}

func TestCacheStoreRejectsEscapingLayoutSymlinkAtStartup(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "v1")); err != nil {
		t.Fatal(err)
	}
	if store, err := newCacheStore(dir, 1<<20, 100); err == nil {
		store.close()
		t.Fatal("cache accepted an escaping layout symlink")
	}
}

func stringsRepeat(value string, count int) string {
	out := ""
	for i := 0; i < count; i++ {
		out += value
	}
	return out
}

func TestCacheQuotaNeverEvictsPinsButRetentionDoes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	store, err := newCacheStore(dir, 4096, 8)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	keyA := canonicalCacheKey("p", "s", "GET", "a", "", nil)
	keyB := canonicalCacheKey("p", "s", "GET", "b", "", nil)
	if err := store.put(testMetadata(keyA, "fuel", now), make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	if err := store.pin(keyA, "pack", true); err != nil {
		t.Fatal(err)
	}
	if err := store.put(testMetadata(keyB, "fuel", now.Add(time.Second)), make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.get("fuel", keyA); !ok {
		t.Fatal("pinned entry was evicted")
	}

	store.mu.Lock()
	meta := *store.entries[keyA]
	store.mu.Unlock()
	meta.DeleteNoLaterThan = now.Add(time.Minute)
	if err := store.updateMetadata(meta); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now.Add(2 * time.Minute) }
	keyC := canonicalCacheKey("p", "s", "GET", "c", "", nil)
	if err := store.put(testMetadata(keyC, "fuel", now), make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.get("fuel", keyA); ok {
		t.Fatal("pin extended mandatory retention")
	}
}

func TestCachePinReconciliationIsExactAndIdempotent(t *testing.T) {
	store, err := newCacheStore(t.TempDir(), 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	keys := []string{
		canonicalCacheKey("p", "s", http.MethodGet, "kept", "", nil),
		canonicalCacheKey("p", "s", http.MethodGet, "orphaned", "", nil),
		canonicalCacheKey("p", "s", http.MethodGet, "expired", "", nil),
	}
	for _, key := range keys {
		meta := testMetadata(key, "places", now)
		if key == keys[2] {
			meta.StaleUntil = now.Add(time.Minute)
		}
		if err := store.put(meta, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	for key, pin := range map[string]string{keys[0]: "old-owner", keys[1]: "ghost-owner", keys[2]: "ghost-owner"} {
		if err := store.pin(key, pin, true); err != nil {
			t.Fatal(err)
		}
	}
	store.now = func() time.Time { return now.Add(2 * time.Minute) }
	missingKey := canonicalCacheKey("p", "s", http.MethodGet, "missing", "", nil)
	desired := map[string][]string{
		keys[0]:     {"pack-b", "pack-a", "pack-a"},
		keys[2]:     {"pack-a"},
		missingKey:  {"pack-b"},
		"not-a-key": {"pack-a"},
	}
	unavailable, changed, err := store.reconcilePins(desired)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 3 {
		t.Fatalf("changed sidecars = %d, want 3", changed)
	}
	for _, key := range []string{keys[2], missingKey, "not-a-key"} {
		if _, ok := unavailable[key]; !ok {
			t.Errorf("unavailable keys missing %q", key)
		}
	}
	store.mu.Lock()
	keptPins := append([]string(nil), store.entries[keys[0]].Pins...)
	orphanedPins := append([]string(nil), store.entries[keys[1]].Pins...)
	expiredPins := append([]string(nil), store.entries[keys[2]].Pins...)
	indexedBytes := int64(0)
	for _, meta := range store.entries {
		indexedBytes += meta.Length + meta.metadataLength
	}
	store.mu.Unlock()
	if fmt.Sprint(keptPins) != "[pack-a pack-b]" || len(orphanedPins) != 0 || len(expiredPins) != 0 {
		t.Fatalf("reconciled pins: kept=%v orphaned=%v expired=%v", keptPins, orphanedPins, expiredPins)
	}
	if stats := store.stats(); stats.Bytes != indexedBytes {
		t.Fatalf("cache bytes = %d, indexed metadata = %d", stats.Bytes, indexedBytes)
	}
	if _, changed, err := store.reconcilePins(desired); err != nil || changed != 0 {
		t.Fatalf("idempotent reconciliation changed %d sidecars: %v", changed, err)
	}
}

func TestCachePinOwnerReconciliationPreservesOtherOwners(t *testing.T) {
	store, err := newCacheStore(t.TempDir(), 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	now := time.Now().UTC()
	key := canonicalCacheKey("p", "s", http.MethodGet, "owned", "", nil)
	newKey := canonicalCacheKey("p", "s", http.MethodGet, "new-owner", "", nil)
	missingKey := canonicalCacheKey("p", "s", http.MethodGet, "missing-owner", "", nil)
	for _, cacheKey := range []string{key, newKey} {
		if err := store.put(testMetadata(cacheKey, "places", now), []byte("owned")); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.pin(key, "pack-owner", true); err != nil {
		t.Fatal(err)
	}
	ownerUnavailable, changed, err := store.reconcilePinOwner("internal-owner", []string{key, missingKey})
	if err != nil || changed != 1 {
		t.Fatalf("owner reconciliation changed %d sidecars: unavailable=%v err=%v", changed, ownerUnavailable, err)
	}
	if _, missing := ownerUnavailable[missingKey]; !missing {
		t.Fatalf("missing key was available: %v", ownerUnavailable)
	}
	store.mu.Lock()
	ownerPins := append([]string(nil), store.entries[key].Pins...)
	store.mu.Unlock()
	if fmt.Sprint(ownerPins) != "[internal-owner pack-owner]" {
		t.Fatalf("owner reconciliation replaced other pins: %v", ownerPins)
	}
	if _, changed, err := store.addPinOwner("internal-owner", []string{newKey}); err != nil || changed != 1 {
		t.Fatalf("owner addition changed %d sidecars: %v", changed, err)
	}
	store.mu.Lock()
	oldPins := append([]string(nil), store.entries[key].Pins...)
	newPins := append([]string(nil), store.entries[newKey].Pins...)
	store.mu.Unlock()
	if !containsString(oldPins, "internal-owner") || !containsString(newPins, "internal-owner") {
		t.Fatalf("owner addition was not two-phase: old=%v new=%v", oldPins, newPins)
	}
	if _, changed, err := store.reconcilePinOwner("internal-owner", []string{newKey}); err != nil || changed != 1 {
		t.Fatalf("owner replacement changed %d sidecars: %v", changed, err)
	}
	if _, changed, err := store.reconcilePinOwner("internal-owner", nil); err != nil || changed != 1 {
		t.Fatalf("owner removal changed %d sidecars: %v", changed, err)
	}
}

func TestCacheDeletionFailurePreservesAccounting(t *testing.T) {
	store, err := newCacheStore(t.TempDir(), 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	key := canonicalCacheKey("p", "s", http.MethodGet, "delete-failure", "", nil)
	if err := store.put(testMetadata(key, "places", time.Now().UTC()), []byte("retained")); err != nil {
		t.Fatal(err)
	}
	before := store.stats()
	store.removeFile = func(string) error { return errors.New("forced remove failure") }
	if removed, err := store.clear("places"); err == nil || removed != 0 {
		t.Fatalf("clear = %d, %v; want 0 and error", removed, err)
	}
	after := store.stats()
	if after.Entries != before.Entries || after.Bytes != before.Bytes {
		t.Fatalf("accounting changed after failed deletion: before=%+v after=%+v", before, after)
	}
	store.mu.Lock()
	_, retained := store.entries[key]
	store.mu.Unlock()
	if !retained {
		t.Fatal("failed deletion dropped the cache index entry")
	}
}

func TestCacheEvictionFailureRejectsAdmissionAndPreservesAccounting(t *testing.T) {
	store, err := newCacheStore(t.TempDir(), 2000, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	now := time.Now().UTC()
	first := canonicalCacheKey("p", "s", http.MethodGet, "first", "", nil)
	second := canonicalCacheKey("p", "s", http.MethodGet, "second", "", nil)
	if err := store.put(testMetadata(first, "places", now), make([]byte, 800)); err != nil {
		t.Fatal(err)
	}
	before := store.stats()
	store.removeFile = func(string) error { return errors.New("forced eviction failure") }
	if err := store.put(testMetadata(second, "places", now.Add(time.Second)), make([]byte, 800)); err == nil || !strings.Contains(err.Error(), "evict") {
		t.Fatalf("second admission error = %v", err)
	}
	after := store.stats()
	if after.Entries != before.Entries || after.Bytes != before.Bytes {
		t.Fatalf("failed eviction changed accounting: before=%+v after=%+v", before, after)
	}
	if _, ok := store.get("places", first); !ok {
		t.Fatal("failed eviction dropped the existing entry")
	}
}
