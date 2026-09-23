package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoutingSummaryIsExplicitAndAdvisory(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{GPXDir: t.TempDir(), RoutingCacheDir: dir})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	before := s.broom.status(t.Context())
	assert.False(t, before.SummaryKnown)
	assert.Empty(t, before.SummaryUpdatedAt)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "partial"), []byte("1234"), 0600))
	rec := do(t, s, http.MethodGet, "/offline/routing?summary=1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var measured broomRoutingStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &measured))
	assert.True(t, measured.SummaryKnown)
	assert.NotEmpty(t, measured.SummaryUpdatedAt)
	assert.GreaterOrEqual(t, measured.SummaryBytes, int64(4))
	assert.Empty(t, measured.InventoryUpdatedAt, "a summary must not claim verified ownership counts")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "partial"), []byte("123456789"), 0600))
	polled := s.broom.status(t.Context())
	assert.Equal(t, measured.SummaryBytes, polled.SummaryBytes, "polling must not traverse disk")
	assert.Equal(t, measured.SummaryUpdatedAt, polled.SummaryUpdatedAt)
}

func TestRoutingProgressNeverInspectsArtifactContents(t *testing.T) {
	s, err := New(Config{GPXDir: t.TempDir(), RoutingCacheDir: t.TempDir()})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	s.broom.inspectCache = func(ctx context.Context) (broom.CacheInventory, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return broom.CacheInventory{}, ctx.Err()
		}
		return broom.CacheInventory{Bytes: 1234, PinnedBytes: 1000, InUseBytes: 900, ReclaimableBytes: 234}, nil
	}
	read := func(path string) broomRoutingStatus {
		t.Helper()
		rec := do(t, s, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var status broomRoutingStatus
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
		return status
	}
	for range 10 {
		assert.Empty(t, read("/offline/routing").InventoryUpdatedAt)
	}
	assert.Zero(t, calls.Load())
	full := make(chan *httptest.ResponseRecorder, 1)
	go func() { full <- do(t, s, http.MethodGet, "/offline/routing?inventory=1", nil) }()
	<-started
	// The full scan is deliberately blocked. Progress must still be available.
	progress := make(chan broomRoutingStatus, 1)
	go func() { progress <- s.broom.status(t.Context()) }()
	select {
	case status := <-progress:
		assert.Empty(t, status.InventoryUpdatedAt)
	case <-time.After(time.Second):
		close(release)
		t.Fatal("progress blocked on a full inventory scan")
	}
	close(release)
	require.Equal(t, http.StatusOK, (<-full).Code)
	status := read("/offline/routing")
	assert.EqualValues(t, 1234, status.CacheBytes)
	assert.NotEmpty(t, status.InventoryUpdatedAt)
	for range 10 {
		assert.Equal(t, status.InventoryUpdatedAt, read("/offline/routing").InventoryUpdatedAt)
	}
	assert.EqualValues(t, 1, calls.Load(), "polls must reuse the explicit inventory snapshot")
}
