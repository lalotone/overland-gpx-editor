package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testOutbound(t *testing.T, mode offlineMode, dir string, client *http.Client) (*outboundClient, *cacheStore, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	store, err := newCacheStore(dir, 8<<20, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	return newOutboundClient(mode, store, client, ctx, wg, "gpx-editor/test (test@example.invalid)"), store, cancel, wg
}

func testPolicy(t *testing.T, rawURL string) *providerPolicy {
	t.Helper()
	base, err := parseProviderURL("test", rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return newProviderPolicy("test", "places", base, time.Hour, 24*time.Hour, 7*24*time.Hour, true, 1<<20, []string{"application/json"}, nil, false)
}

func TestOfflineModeTransitionCancelsAndDrainsGeneration(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	modes := newOfflineModeController(root, modeAuto)
	network, release, online := modes.networkContext(context.Background())
	if !online {
		t.Fatal("auto mode refused a network generation")
	}

	transition := make(chan error, 1)
	go func() {
		_, err := modes.set(modeCacheOnly)
		transition <- err
	}()
	select {
	case <-network.Done():
	case <-time.After(time.Second):
		t.Fatal("cache-only transition did not cancel the active generation")
	}
	select {
	case err := <-transition:
		t.Fatalf("transition returned before the active generation drained: %v", err)
	default:
	}
	if _, _, allowed := modes.networkContext(context.Background()); allowed {
		t.Fatal("new network generation started while cache-only transition was draining")
	}

	release()
	release()
	select {
	case err := <-transition:
		if err != nil {
			t.Fatalf("cache-only transition: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cache-only transition did not finish after the generation drained")
	}
}

func TestCacheControlPrecedenceAndRevalidation(t *testing.T) {
	tests := []struct {
		value      string
		want       time.Duration
		revalidate bool
	}{
		{value: "max-age=300, no-cache", want: 0, revalidate: true},
		{value: "max-age=300, s-maxage=60", want: time.Minute},
		{value: "s-maxage=60, max-age=300", want: time.Minute},
		{value: "max-age=300, must-revalidate", want: 5 * time.Minute, revalidate: true},
	}
	for _, tt := range tests {
		got, ok := cacheMaxAge(tt.value)
		if !ok || got != tt.want {
			t.Errorf("cacheMaxAge(%q) = %s, %v; want %s, true", tt.value, got, ok, tt.want)
		}
		if got := requiresRevalidation(tt.value); got != tt.revalidate {
			t.Errorf("requiresRevalidation(%q) = %v", tt.value, got)
		}
	}
}

func TestProviderDeduplicatesAndWaiterCancellationDoesNotPoisonFetch(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	outbound, _, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), upstream.Client())
	defer func() { cancel(); wg.Wait() }()
	request := cachedRequest{policy: testPolicy(t, upstream.URL), method: http.MethodGet, url: upstream.URL, params: "same", cacheable: true}

	ownerDone := make(chan error, 1)
	go func() { _, err := outbound.do(context.Background(), request); ownerDone <- err }()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() { _, err := outbound.do(waiterCtx, request); waiterDone <- err }()
	cancelWaiter()
	if err := <-waiterDone; err == nil {
		t.Fatal("cancelled waiter did not return")
	}
	close(release)
	if err := <-ownerDone; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	response, err := outbound.do(context.Background(), request)
	if err != nil || response.State != "hit" {
		t.Fatalf("cached response = %s, %v", response.State, err)
	}
}

func TestProviderConditional304NoStoreAndStaleOnError(t *testing.T) {
	var calls atomic.Int64
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if call == 2 && r.Header.Get("If-None-Match") == `"v1"` {
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "max-age=0")
		fmt.Fprint(w, `{"version":1}`)
	}))
	defer upstream.Close()
	outbound, store, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), upstream.Client())
	defer func() { cancel(); wg.Wait() }()
	request := cachedRequest{policy: testPolicy(t, upstream.URL), method: http.MethodGet, url: upstream.URL, params: "etag", cacheable: true}
	if response, err := outbound.do(context.Background(), request); err != nil || response.State != "miss" {
		t.Fatalf("first = %s, %v", response.State, err)
	}
	if response, err := outbound.do(context.Background(), request); err != nil || response.State != "revalidated" {
		t.Fatalf("second = %s, %v", response.State, err)
	}

	// Force the entry stale again, then prove a retryable outage serves it.
	key := canonicalCacheKey("test", request.policy.sourceFingerprint, http.MethodGet, "etag", "", nil)
	entry, ok := store.get("places", key)
	if !ok {
		t.Fatal("entry missing")
	}
	entry.Meta.FreshUntil = time.Now().Add(-time.Minute)
	if err := store.updateMetadata(entry.Meta); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if response, err := outbound.do(context.Background(), request); err != nil || response.State != "stale" {
		t.Fatalf("stale = %s, %v", response.State, err)
	}

	noStoreRequest := request
	noStoreRequest.params = "no-store"
	noStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, `{}`)
	}))
	defer noStore.Close()
	noStoreRequest.policy = testPolicy(t, noStore.URL)
	noStoreRequest.url = noStore.URL
	noStoreOutbound, noStoreCache, noStoreCancel, noStoreWG := testOutbound(t, modeAuto, t.TempDir(), noStore.Client())
	defer func() { noStoreCancel(); noStoreWG.Wait() }()
	if response, err := noStoreOutbound.do(context.Background(), noStoreRequest); err != nil || response.State != "bypass" {
		t.Fatalf("no-store = %s, %v", response.State, err)
	}
	if noStoreCache.stats().Entries != 0 {
		t.Fatal("no-store response was persisted")
	}
}

func TestCacheOnlyRestartUsesDiskAndNeverCallsTransport(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"cached":true}`)
	}))
	dir := t.TempDir()
	policy := testPolicy(t, upstream.URL)
	online, _, cancelOnline, onlineWG := testOutbound(t, modeAuto, dir, upstream.Client())
	request := cachedRequest{policy: policy, method: http.MethodGet, url: upstream.URL, params: "known", cacheable: true}
	if _, err := online.do(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	cancelOnline()
	onlineWG.Wait()
	upstream.Close()

	transportCalls := atomic.Int64{}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		transportCalls.Add(1)
		return nil, errorsNew("transport must not be called")
	})}
	offline, _, cancelOffline, offlineWG := testOutbound(t, modeCacheOnly, dir, client)
	defer func() { cancelOffline(); offlineWG.Wait() }()
	if response, err := offline.do(context.Background(), request); err != nil || string(response.Body) != `{"cached":true}` {
		t.Fatalf("disk replay = %q, %v", response.Body, err)
	}
	request.params = "new"
	if _, err := offline.do(context.Background(), request); err == nil {
		t.Fatal("new request did not miss")
	}
	if transportCalls.Load() != 0 {
		t.Fatalf("cache-only transport calls = %d", transportCalls.Load())
	}
}

func TestProviderBoundsDistinctPendingWorkBeforeStartingGoroutines(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	outbound, _, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), upstream.Client())
	defer func() { cancel(); wg.Wait() }()
	outbound.maxPending = 2
	outbound.maxPendingPerProvider = 1
	p1 := testPolicy(t, upstream.URL)
	p2 := testPolicy(t, upstream.URL)
	p2.name = "second"
	p2.scope = "pois"

	done := make(chan error, 2)
	go func() {
		_, err := outbound.do(context.Background(), cachedRequest{policy: p1, url: upstream.URL, params: "one"})
		done <- err
	}()
	for calls.Load() != 1 {
		time.Sleep(time.Millisecond)
	}
	_, err := outbound.do(context.Background(), cachedRequest{policy: p1, url: upstream.URL, params: "two"})
	var busy *outboundBusyError
	if !errors.As(err, &busy) || busy.Scope != p1.scope {
		t.Fatalf("per-provider admission error = %v", err)
	}

	go func() {
		_, err := outbound.do(context.Background(), cachedRequest{policy: p2, url: upstream.URL, params: "three"})
		done <- err
	}()
	for calls.Load() != 2 {
		time.Sleep(time.Millisecond)
	}
	p3 := testPolicy(t, upstream.URL)
	p3.name = "third"
	_, err = outbound.do(context.Background(), cachedRequest{policy: p3, url: upstream.URL, params: "four"})
	if !errors.As(err, &busy) {
		t.Fatalf("global admission error = %v", err)
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestProviderFetchHasDeadlineAndStopsOnShutdown(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	for _, tt := range []struct {
		name     string
		shutdown bool
	}{
		{name: "deadline"},
		{name: "shutdown", shutdown: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			outbound, _, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), client)
			outbound.fetchTimeout = 20 * time.Millisecond
			if tt.shutdown {
				outbound.fetchTimeout = time.Minute
			}
			done := make(chan error, 1)
			go func() {
				_, err := outbound.do(context.Background(), cachedRequest{policy: testPolicy(t, "http://example.test"), url: "http://example.test", params: tt.name})
				done <- err
			}()
			if tt.shutdown {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("fetch unexpectedly succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("fetch did not stop")
			}
			cancel()
			wg.Wait()
		})
	}
}

func TestProviderPolicyCanExtendFetchDeadline(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-time.After(50 * time.Millisecond):
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    r,
			}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	outbound, _, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), client)
	defer func() { cancel(); wg.Wait() }()
	outbound.fetchTimeout = 20 * time.Millisecond

	extended := testPolicy(t, "http://example.test")
	extended.fetchTimeout = 100 * time.Millisecond
	if _, err := outbound.do(context.Background(), cachedRequest{policy: extended, url: "http://example.test/extended", params: "extended"}); err != nil {
		t.Fatal(err)
	}

	standard := testPolicy(t, "http://example.test")
	standard.name = "standard"
	if _, err := outbound.do(context.Background(), cachedRequest{policy: standard, url: "http://example.test/standard", params: "standard"}); err == nil {
		t.Fatal("standard request outlived its default deadline")
	}
}

func TestServerUsesContextDeadlinesForOutboundProviders(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	srv, err := New(Config{GPXDir: t.TempDir(), ElevationHost: "http://elevation.invalid", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if srv.outbound.client.Timeout != 0 {
		t.Fatalf("outbound HTTP client timeout = %s, want context-only", srv.outbound.client.Timeout)
	}
	if srv.elevation.client.Timeout != client.Timeout {
		t.Fatalf("elevation HTTP client timeout = %s, want %s", srv.elevation.client.Timeout, client.Timeout)
	}
	if srv.outbound.fetchTimeout != client.Timeout {
		t.Fatalf("default outbound timeout = %s, want injected %s", srv.outbound.fetchTimeout, client.Timeout)
	}
	for _, name := range []string{"valhalla-route", "osrm-route"} {
		if got := srv.providers[name].fetchTimeout; got != routeOutboundFetchTimeout {
			t.Errorf("%s timeout = %s, want %s", name, got, routeOutboundFetchTimeout)
		}
	}
	if got := srv.providers["surface"].fetchTimeout; got != 0 {
		t.Errorf("surface timeout override = %s, want default", got)
	}
}

func TestProviderForcesIdentityAndRejectsEncodedResponses(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Fatalf("Accept-Encoding = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    r,
		}, nil
	})}
	outbound, _, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), client)
	defer func() { cancel(); wg.Wait() }()
	_, err := outbound.do(context.Background(), cachedRequest{policy: testPolicy(t, "http://example.test"), url: "http://example.test", params: "encoded"})
	if err == nil || !strings.Contains(err.Error(), "content encoding") {
		t.Fatalf("encoded response error = %v", err)
	}
}

func TestPrivateResponsesRequireApplicationDataPolicy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", `private = "Set-Cookie", max-age=3600`)
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	outbound, store, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), upstream.Client())
	defer func() { cancel(); wg.Wait() }()
	policy := testPolicy(t, upstream.URL)
	request := cachedRequest{policy: policy, url: upstream.URL, params: "generic", cacheable: true}
	if response, err := outbound.do(context.Background(), request); err != nil || response.State != "bypass" {
		t.Fatalf("generic private response = %q, %v", response.State, err)
	}
	policy.applicationData = true
	request.params = "application-data"
	if response, err := outbound.do(context.Background(), request); err != nil || response.State != "miss" {
		t.Fatalf("application-data private response = %q, %v", response.State, err)
	}
	if store.stats().Entries != 1 {
		t.Fatalf("cache entries = %d, want 1", store.stats().Entries)
	}
}

func TestApplicationDataNeverStoresNoStoreResponses(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer upstream.Close()
	outbound, store, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), upstream.Client())
	defer func() { cancel(); wg.Wait() }()
	policy := testPolicy(t, upstream.URL)
	policy.applicationData = true
	request := cachedRequest{policy: policy, url: upstream.URL, params: "no-store", cacheable: true}
	if response, err := outbound.do(context.Background(), request); err != nil || response.State != "bypass" {
		t.Fatalf("application-data no-store = %q, %v", response.State, err)
	}
	if store.stats().Entries != 0 {
		t.Fatal("application-data no-store response was persisted")
	}

	meta := cacheMetadata{
		Key:   canonicalCacheKey(policy.name, policy.sourceFingerprint, http.MethodGet, "no-store", "", nil),
		Scope: policy.scope, Provider: policy.name, SourceFingerprint: policy.sourceFingerprint,
		Status: http.StatusOK, ContentType: "application/json", ETag: `"v1"`,
		FetchedAt: time.Now().Add(-time.Hour), FreshUntil: time.Now().Add(-time.Minute),
	}
	if err := store.put(meta, []byte(`{"valid":true}`)); err != nil {
		t.Fatal(err)
	}
	if response, err := outbound.do(context.Background(), request); err != nil || response.State != "revalidated" {
		t.Fatalf("application-data 304 no-store = %q, %v", response.State, err)
	}
	if store.stats().Entries != 0 {
		t.Fatal("304 no-store retained the cached response")
	}
}

func TestApplicationDataNoStoreSuccessEvictsStaleEntry(t *testing.T) {
	var noStore atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if noStore.Load() {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "max-age=0")
		}
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	outbound, store, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), upstream.Client())
	defer func() { cancel(); wg.Wait() }()
	policy := testPolicy(t, upstream.URL)
	policy.applicationData = true
	request := cachedRequest{policy: policy, url: upstream.URL, params: "stale-no-store", cacheable: true}
	if response, err := outbound.do(context.Background(), request); err != nil || response.State != "miss" {
		t.Fatalf("initial response = %q, %v", response.State, err)
	}
	noStore.Store(true)
	if response, err := outbound.do(context.Background(), request); err != nil || response.State != "bypass" {
		t.Fatalf("no-store replacement = %q, %v", response.State, err)
	}
	if store.stats().Entries != 0 {
		t.Fatal("200 no-store retained the stale application-data response")
	}
}

func TestResponseValidatorRunsBeforeCacheReplacement(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	outbound, store, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), upstream.Client())
	defer func() { cancel(); wg.Wait() }()
	_, err := outbound.do(context.Background(), cachedRequest{
		policy: testPolicy(t, upstream.URL), url: upstream.URL, params: "invalid", cacheable: true,
		validate: func([]byte) error { return errors.New("invalid schema") },
	})
	if err == nil || store.stats().Entries != 0 {
		t.Fatalf("validator error = %v, entries = %d", err, store.stats().Entries)
	}
}

func TestMalformedSuccessDoesNotReplaceGoodCacheEntry(t *testing.T) {
	var invalid atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=0")
		if invalid.Load() {
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, `{"value":"good"}`)
	}))
	defer upstream.Close()
	outbound, store, cancel, wg := testOutbound(t, modeAuto, t.TempDir(), upstream.Client())
	defer func() { cancel(); wg.Wait() }()
	policy := testPolicy(t, upstream.URL)
	request := cachedRequest{
		policy: policy, url: upstream.URL, params: "replacement", cacheable: true,
		validate: func(body []byte) error {
			if !strings.Contains(string(body), `"value"`) {
				return errors.New("invalid schema")
			}
			return nil
		},
	}
	if _, err := outbound.do(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	invalid.Store(true)
	if _, err := outbound.do(context.Background(), request); err == nil {
		t.Fatal("malformed success was accepted")
	}
	key := canonicalCacheKey(policy.name, policy.sourceFingerprint, http.MethodGet, "replacement", "", nil)
	entry, ok := store.get(policy.scope, key)
	if !ok || string(entry.Body) != `{"value":"good"}` {
		t.Fatalf("good cache entry was replaced: found=%v body=%q", ok, entry.Body)
	}
}

type stringError string

func (e stringError) Error() string { return string(e) }
func errorsNew(value string) error  { return stringError(value) }

func TestRateGroupSerializesStartsWithoutSlowSuite(t *testing.T) {
	group := newRateGroup(5 * time.Millisecond)
	var mu sync.Mutex
	var starts []time.Time
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := group.acquire(context.Background())
			if err != nil {
				return
			}
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()
	sortTimes(starts)
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 4*time.Millisecond {
			t.Errorf("starts only %s apart", starts[i].Sub(starts[i-1]))
		}
	}
}

func sortTimes(values []time.Time) {
	for i := range values {
		for j := i + 1; j < len(values); j++ {
			if values[j].Before(values[i]) {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
}
