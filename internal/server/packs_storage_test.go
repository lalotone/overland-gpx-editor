package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManualPacksAreLimitedByStorageNotCount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Fecha":"today","ListaEESSPrecio":[]}`)
	}))
	t.Cleanup(upstream.Close)
	s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), FuelURL: upstream.URL})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	for i := range 40 {
		pack, _, err := s.packs.start(packInput{Name: fmt.Sprintf("saved %d", i), BBox: &bbox{South: 40, West: -1, North: 41, East: 0}, Scopes: []string{"fuel"}})
		require.NoError(t, err)
		waitForPackState(t, s.packs, pack.ID, "complete")
	}
	assert.Len(t, s.packs.summaries(), 40)
	entries, err := os.ReadDir(s.cache.packsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 40)
	assert.Less(t, s.cache.stats().Reserved-s.packs.controlBaseReserve(), int64(100<<10), "completed packs reserve their actual metadata, not 40 maximum-size files")
}

func TestIncompletePackRetryReusesIdentityAndPins(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/fuel" {
			fmt.Fprint(w, `{"Fecha":"today","ListaEESSPrecio":[]}`)
		} else if fail.Load() {
			w.WriteHeader(http.StatusBadRequest)
		} else {
			fmt.Fprint(w, `{"elements":[]}`)
		}
	}))
	t.Cleanup(upstream.Close)
	s, err := New(Config{GPXDir: t.TempDir(), OfflineCacheDir: t.TempDir(), FuelURL: upstream.URL + "/fuel", OverpassURL: upstream.URL + "/pois"})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	s.providers["pois"].group = newRateGroup(0)
	input := packInput{Name: "Map: Cataluña", Regional: true, BBox: &bbox{South: 40, West: -1, North: 40.1, East: -0.9}, Scopes: []string{"fuel", "pois"}}
	first, _, err := s.packs.start(input)
	require.NoError(t, err)
	waitForPackState(t, s.packs, first.ID, "incomplete")
	require.Eventually(t, func() bool {
		s.packs.mu.Lock()
		defer s.packs.mu.Unlock()
		return !s.packs.packs[first.ID].working
	}, time.Second, time.Millisecond)
	fuel := s.cache.keysForScope("fuel")
	require.Len(t, fuel, 1)
	input.Name, input.CoverageKind = "Visible area near Cataluña", "area"
	// A failed retry checkpoint must leave the old manifest, pins and storage
	// accounting intact rather than replacing it with a half-started attempt.
	reserved := s.cache.stats().Reserved
	savedDir := s.cache.packsDir + ".saved"
	require.NoError(t, os.Rename(s.cache.packsDir, savedDir))
	require.NoError(t, os.Symlink(t.TempDir(), s.cache.packsDir))
	_, _, err = s.packs.start(input)
	require.Error(t, err)
	assert.Equal(t, reserved, s.cache.stats().Reserved)
	previous, ok := s.packs.publicManifest(first.ID)
	require.True(t, ok)
	assert.Equal(t, "incomplete", previous.State)
	require.NoError(t, os.Remove(s.cache.packsDir))
	require.NoError(t, os.Rename(savedDir, s.cache.packsDir))
	fail.Store(false)
	retry, _, err := s.packs.start(input)
	require.NoError(t, err)
	assert.Equal(t, first.ID, retry.ID)
	assert.Equal(t, first.CreatedAt, retry.CreatedAt)
	entry, ok := s.cache.get("fuel", fuel[0])
	require.True(t, ok)
	assert.Equal(t, []string{first.ID}, entry.Meta.Pins, "retry must keep ownership without adding another pin")
	status := waitForPackState(t, s.packs, retry.ID, "complete")
	assert.Equal(t, 1, status.Resources[packResourceFuelPrices].Reused)
	assert.Len(t, s.packs.summaries(), 1)
	entries, err := os.ReadDir(s.cache.packsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestManifestStorageReservationTracksGrowthAndDeletion(t *testing.T) {
	s := newPackTestServer(t)
	base := s.cache.stats().Reserved
	id, err := newPackID()
	require.NoError(t, err)
	pack := &packManifest{ID: id, State: "running", Input: packInput{Regional: true}}
	require.NoError(t, s.packs.persistLocked(pack))
	assert.Equal(t, base+maxRegionalManifestBytes, s.cache.stats().Reserved)
	pack.State = "complete"
	require.NoError(t, s.packs.persistLocked(pack))
	assert.Less(t, s.cache.stats().Reserved, base+1024)
	require.NoError(t, s.packs.removeManifestFile(id))
	assert.Equal(t, base, s.cache.stats().Reserved)
}
