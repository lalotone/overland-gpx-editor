package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBulkAdmissionOutlivesProviderTimeout(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)
	out, store, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), upstream.Client())
	t.Cleanup(func() { cancel(); wg.Wait() })
	policy := testPolicy(t, upstream.URL)
	policy.fetchTimeout = 100 * time.Millisecond
	policy.group = newRateGroup(0)
	store.admissionWindow, store.admissions = time.Now(), 1000
	waiting, resume := make(chan struct{}), make(chan struct{})
	store.waitAdmission = func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		// Pass the entire network timeout while waiting locally.
		if err := waitCacheAdmission(ctx, 2*policy.fetchTimeout); err != nil {
			return err
		}
		select {
		case <-resume:
		case <-ctx.Done():
			return ctx.Err()
		}
		store.mu.Lock()
		store.admissionWindow = store.admissionWindow.Add(-time.Minute)
		store.mu.Unlock()
		return nil
	}
	budget := &packAdmissionBudget{limit: 8 << 20}
	done := make(chan error, 1)
	go func() {
		_, err := out.do(t.Context(), cachedRequest{policy: policy, url: upstream.URL, params: "bulk", cacheable: true, admit: budget.admit, cancelWithCaller: true})
		done <- err
	}()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("request did not reach admission")
	}
	// Local waiting releases the provider slot so passive traffic can proceed.
	response, err := out.do(t.Context(), cachedRequest{policy: policy, url: upstream.URL, params: "passive", cacheable: true})
	require.NoError(t, err)
	assert.Equal(t, "bypass", response.State)
	close(resume)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("bulk request did not resume")
	}
	assert.EqualValues(t, 2, calls.Load(), "bulk response must not be downloaded again")
	assert.Positive(t, budget.used)
}

func TestBulkCacheAdmissionContinuesAfterWindow(t *testing.T) {
	store, err := newCacheStore(t.TempDir(), 8<<20, 200000)
	require.NoError(t, err)
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	budget := &packAdmissionBudget{limit: 8 << 20}
	var admitted int64
	for i := range 1000 {
		meta := testMetadata(fmt.Sprintf("%064x", i), "places", now)
		n, err := store.putForRequest(t.Context(), meta, []byte(`{}`), budget.admit)
		require.NoError(t, err)
		admitted += n
	}
	meta := testMetadata(fmt.Sprintf("%064x", 1000), "places", now)
	var rateErr *cacheAdmissionRateError
	for range 3 {
		_, err := store.putWithAdmission(meta, []byte(`{}`), budget.admit)
		require.ErrorAs(t, err, &rateErr)
		assert.Equal(t, admitted, budget.used, "rejections must not charge the budget")
	}
	// Passive admission remains protected, without invoking the bulk wait.
	_, err = store.putForRequest(t.Context(), meta, []byte(`{}`), nil)
	require.ErrorAs(t, err, &rateErr)
	waits := 0
	store.waitAdmission = func(ctx context.Context, delay time.Duration) error {
		waits++
		assert.Equal(t, time.Minute, delay)
		now = now.Add(delay)
		return ctx.Err()
	}
	n, err := store.putForRequest(t.Context(), meta, []byte(`{}`), budget.admit)
	require.NoError(t, err)
	assert.Equal(t, 1, waits)
	assert.Equal(t, admitted+n, budget.used)
	assert.Len(t, store.entries, 1001)
}

func TestBulkCacheAdmissionCancellation(t *testing.T) {
	store, err := newCacheStore(t.TempDir(), 8<<20, 200000)
	require.NoError(t, err)
	store.admissionWindow, store.admissions = time.Now(), 1000
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiting := make(chan struct{})
	store.waitAdmission = func(ctx context.Context, delay time.Duration) error {
		close(waiting)
		return waitCacheAdmission(ctx, delay)
	}
	budget := &packAdmissionBudget{limit: 8 << 20}
	done := make(chan error, 1)
	go func() {
		_, err := store.putForRequest(ctx, testMetadata(fmt.Sprintf("%064x", 1), "places", time.Now()), []byte(`{}`), budget.admit)
		done <- err
	}()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("bulk request did not wait")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("admission wait did not cancel")
	}
	assert.Zero(t, budget.used)
	assert.Empty(t, store.entries)
}

func TestBulkCacheStorageLimitsDoNotWait(t *testing.T) {
	for _, kind := range []string{"entry", "bytes", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			store, err := newCacheStore(t.TempDir(), 8<<20, 200000)
			require.NoError(t, err)
			now := time.Now()
			first := testMetadata(fmt.Sprintf("%064x", 1), "places", now)
			require.NoError(t, store.put(first, []byte(`{}`)))
			require.NoError(t, store.pin(first.Key, "pack", true))
			switch kind {
			case "entry":
				store.maxEntries = 1
			case "bytes":
				store.maxBytes = store.bytes + 1
			case "oversized":
				store.maxBytes = 1
			}
			store.waitAdmission = func(context.Context, time.Duration) error {
				t.Error("storage exhaustion must not wait")
				return errors.New("unexpected wait")
			}
			budget := &packAdmissionBudget{limit: 8 << 20}
			_, err = store.putForRequest(t.Context(), testMetadata(fmt.Sprintf("%064x", 2), "places", now), []byte(`{}`), budget.admit)
			var storageErr cacheStorageLimitError
			require.ErrorAs(t, err, &storageErr)
			assert.Zero(t, budget.used)
		})
	}
}
