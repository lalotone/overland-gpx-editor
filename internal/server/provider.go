package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type offlineMode string

const (
	modeAuto      offlineMode = "auto"
	modeCacheOnly offlineMode = "cache-only"

	defaultMaxPendingOutbound            = 64
	defaultMaxPendingOutboundPerProvider = 16
	defaultOutboundFetchTimeout          = 30 * time.Second
)

// offlineModeController linearizes runtime mode changes with transport starts.
// Cache lookups remain available in either mode; only network generations are
// cancelled when the user chooses cache-only.
type offlineModeController struct {
	mu         sync.Mutex
	root       context.Context
	configured offlineMode
	current    offlineMode
	generation context.Context
	cancel     context.CancelFunc
	active     int
	cond       *sync.Cond
}

func newOfflineModeController(root context.Context, mode offlineMode) *offlineModeController {
	controller := &offlineModeController{root: root, configured: mode, current: mode}
	controller.cond = sync.NewCond(&controller.mu)
	if mode == modeAuto {
		controller.generation, controller.cancel = context.WithCancel(root)
	}
	return controller
}

func (c *offlineModeController) mode() offlineMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *offlineModeController) canToggle() bool { return c.configured == modeAuto }

func (c *offlineModeController) set(mode offlineMode) (bool, error) {
	if mode != modeAuto && mode != modeCacheOnly {
		return false, errors.New("offline mode must be auto or cache-only")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if mode == modeAuto && c.configured == modeCacheOnly {
		return false, errors.New("startup cache-only mode cannot be overridden")
	}
	if c.current == mode {
		return false, nil
	}
	if mode == modeCacheOnly {
		c.current = mode
		if c.cancel != nil {
			c.cancel()
		}
		for c.active != 0 {
			c.cond.Wait()
		}
		c.generation = nil
		c.cancel = nil
		return true, nil
	}
	c.generation, c.cancel = context.WithCancel(c.root)
	c.current = mode
	return true, nil
}

func (c *offlineModeController) networkContext(parent context.Context) (context.Context, func(), bool) {
	c.mu.Lock()
	if c.current == modeCacheOnly || c.generation == nil {
		c.mu.Unlock()
		return nil, func() {}, false
	}
	ctx, cancel := context.WithCancel(c.generation)
	stop := context.AfterFunc(parent, cancel)
	if parent.Err() != nil {
		cancel()
	}
	c.active++
	c.mu.Unlock()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stop()
			cancel()
			c.mu.Lock()
			c.active--
			c.cond.Broadcast()
			c.mu.Unlock()
		})
	}, true
}

type offlineMissError struct{ Scope string }

func (e *offlineMissError) Error() string { return "resource is not available in the offline cache" }

type upstreamStatusError struct {
	Provider string
	Status   int
}

type outboundBusyError struct{ Scope string }

func (e *outboundBusyError) Error() string { return "outbound request queue is full" }

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("%s returned %d", e.Provider, e.Status)
}

type providerPolicy struct {
	name              string
	scope             string
	baseURL           *url.URL
	sourceFingerprint string
	fallbackFresh     time.Duration
	maxStale          time.Duration // zero permits dated stale indefinitely
	allowStale        bool
	retention         time.Duration // zero has no provider ceiling
	maxFresh          time.Duration // zero honors provider freshness without a cap
	staleOnError      bool
	maxBody           int64
	contentTypes      []string
	group             *rateGroup
	packEligible      bool
	applicationData   bool
	approvedHosts     map[string]struct{}
	fetchTimeout      time.Duration
}

type cachedRequest struct {
	policy           *providerPolicy
	method           string
	url              string
	params           string
	language         string
	body             []byte
	headers          map[string]string
	cacheable        bool
	validate         func([]byte) error
	admit            func(int64) error
	cancelWithCaller bool
}

type cachedResponse struct {
	Status        int
	Headers       map[string]string
	Body          []byte
	State         string
	Meta          cacheMetadata
	Key           string
	AdmittedBytes int64
}

type inflightFetch struct {
	done chan struct{}
	resp cachedResponse
	err  error
}

type providerHealth struct {
	Failures    int       `json:"failures"`
	CircuitOpen bool      `json:"circuitOpen"`
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
}

type outboundStats struct {
	CacheHits        uint64
	CacheMisses      uint64
	CacheStale       uint64
	CacheRevalidated uint64
	CacheBypass      uint64
	OfflineMisses    uint64
	QueueRejected    uint64
	NetworkRequests  uint64
	NetworkFailures  uint64
	Status2xx        uint64
	Status3xx        uint64
	Status4xx        uint64
	Status5xx        uint64
	Duration         time.Duration
	MaxDuration      time.Duration
	InFlight         int
	PeakInFlight     int
}

type healthState struct {
	failures    int
	openUntil   time.Time
	lastSuccess time.Time
}

type outboundClient struct {
	modes     *offlineModeController
	store     *cacheStore
	client    *http.Client
	ctx       context.Context
	wg        *sync.WaitGroup
	userAgent string
	referer   string
	now       func() time.Time

	mu       sync.Mutex
	inflight map[string]*inflightFetch
	health   map[string]*healthState
	pending  map[string]int
	stats    outboundStats

	maxPending            int
	maxPendingPerProvider int
	fetchTimeout          time.Duration
}

func newOutboundClient(mode offlineMode, store *cacheStore, client *http.Client, ctx context.Context, wg *sync.WaitGroup, userAgent string) *outboundClient {
	return newOutboundClientWithModes(newOfflineModeController(ctx, mode), store, client, ctx, wg, userAgent)
}

func newOutboundClientWithModes(modes *offlineModeController, store *cacheStore, client *http.Client, ctx context.Context, wg *sync.WaitGroup, userAgent string) *outboundClient {
	return &outboundClient{
		modes: modes, store: store, client: client, ctx: ctx, wg: wg, userAgent: userAgent,
		referer: "https://github.com/lalotone/overland-gpx-editor", now: time.Now,
		inflight: make(map[string]*inflightFetch), health: make(map[string]*healthState), pending: make(map[string]int),
		maxPending: defaultMaxPendingOutbound, maxPendingPerProvider: defaultMaxPendingOutboundPerProvider,
		fetchTimeout: defaultOutboundFetchTimeout,
	}
}

func parseProviderURL(name, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s URL must be an absolute http or https URL without credentials, query, or fragment", name)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func sourceFingerprint(u *url.URL) string {
	return canonicalCacheKey("source", strings.ToLower(u.Scheme+"://"+u.Host), http.MethodGet, u.EscapedPath(), "", nil)
}

func (o *outboundClient) do(ctx context.Context, request cachedRequest) (response cachedResponse, err error) {
	defer func() { o.recordResult(response, err) }()
	p := request.policy
	if p == nil || p.baseURL == nil {
		return cachedResponse{}, errors.New("provider is not configured")
	}
	if request.method == "" {
		request.method = http.MethodGet
	}
	request.cacheable = request.cacheable && o.store != nil && o.store.writable
	key := canonicalCacheKey(p.name, p.sourceFingerprint, request.method, request.params, request.language, request.body)
	now := o.now().UTC()
	var stale *cacheEntry
	if request.cacheable {
		if entry, ok := o.store.get(p.scope, key); ok && retainedAt(entry.Meta, now) {
			if now.Before(entry.Meta.FreshUntil) {
				return responseFromEntry(entry, key, "hit"), nil
			}
			stale = entry
		}
	}
	if o.modes.mode() == modeCacheOnly {
		if stale != nil && staleAllowed(stale.Meta, now) {
			return responseFromEntry(stale, key, "stale"), nil
		}
		return cachedResponse{}, &offlineMissError{Scope: p.scope}
	}

	o.mu.Lock()
	if existing := o.inflight[key]; existing != nil {
		o.mu.Unlock()
		select {
		case <-existing.done:
			return existing.resp, existing.err
		case <-ctx.Done():
			return cachedResponse{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		o.mu.Unlock()
		return cachedResponse{}, err
	}
	if len(o.inflight) >= o.maxPending || o.pending[p.name] >= o.maxPendingPerProvider {
		o.mu.Unlock()
		return cachedResponse{}, &outboundBusyError{Scope: p.scope}
	}
	fetch := &inflightFetch{done: make(chan struct{})}
	o.inflight[key] = fetch
	o.pending[p.name]++
	if len(o.inflight) > o.stats.PeakInFlight {
		o.stats.PeakInFlight = len(o.inflight)
	}
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		fetchParent := o.ctx
		if request.cancelWithCaller {
			fetchParent = ctx
		}
		fetchTimeout := o.fetchTimeout
		if request.policy.fetchTimeout > 0 {
			fetchTimeout = request.policy.fetchTimeout
		}
		fetchCtx, cancel := context.WithTimeout(fetchParent, fetchTimeout)
		defer cancel()
		networkCtx, release, online := o.modes.networkContext(fetchCtx)
		if !online {
			if stale != nil && staleAllowed(stale.Meta, o.now().UTC()) {
				fetch.resp = responseFromEntry(stale, key, "stale")
			} else {
				fetch.err = &offlineMissError{Scope: p.scope}
			}
		} else {
			fetch.resp, fetch.err = o.fetch(networkCtx, request, key, stale)
			release()
			if fetch.err != nil && o.modes.mode() == modeCacheOnly {
				if stale != nil && staleAllowed(stale.Meta, o.now().UTC()) {
					fetch.resp = responseFromEntry(stale, key, "stale")
					fetch.err = nil
				} else if errors.Is(fetch.err, context.Canceled) {
					fetch.err = &offlineMissError{Scope: p.scope}
				}
			}
		}
		o.mu.Lock()
		delete(o.inflight, key)
		o.pending[p.name]--
		close(fetch.done)
		o.mu.Unlock()
	}()
	o.mu.Unlock()

	select {
	case <-fetch.done:
		return fetch.resp, fetch.err
	case <-ctx.Done():
		return cachedResponse{}, ctx.Err()
	}
}

func retainedAt(meta cacheMetadata, now time.Time) bool {
	return meta.DeleteNoLaterThan.IsZero() || now.Before(meta.DeleteNoLaterThan)
}

func staleAllowed(meta cacheMetadata, now time.Time) bool {
	return retainedAt(meta, now) && (meta.StaleUntil.IsZero() || now.Before(meta.StaleUntil))
}

func responseFromEntry(entry *cacheEntry, key, state string) cachedResponse {
	return cachedResponse{Status: entry.Meta.Status, Headers: cloneStringMap(entry.Meta.Headers), Body: entry.Body, State: state, Meta: entry.Meta, Key: key}
}

func (o *outboundClient) fetch(ctx context.Context, request cachedRequest, key string, stale *cacheEntry) (cachedResponse, error) {
	p := request.policy
	now := o.now().UTC()
	requestURL, err := url.Parse(request.url)
	if err != nil || !strings.EqualFold(requestURL.Scheme, p.baseURL.Scheme) {
		return cachedResponse{}, errors.New("provider request left approved origin")
	}
	if _, approved := p.approvedHosts[strings.ToLower(requestURL.Host)]; !approved {
		return cachedResponse{}, errors.New("provider request left approved origin")
	}
	if o.circuitOpen(p.name, now) {
		if stale != nil && p.staleOnError && staleAllowed(stale.Meta, now) {
			return responseFromEntry(stale, key, "stale"), nil
		}
		return cachedResponse{}, errors.New("upstream temporarily unavailable")
	}
	if p.group != nil {
		release, err := p.group.acquire(ctx)
		if err != nil {
			return cachedResponse{}, err
		}
		defer release()
	}

	req, err := http.NewRequestWithContext(ctx, request.method, request.url, bytes.NewReader(request.body))
	if err != nil {
		return cachedResponse{}, err
	}
	req.Header.Set("User-Agent", o.userAgent)
	req.Header.Set("Referer", o.referer)
	for name, value := range request.headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if stale != nil {
		if stale.Meta.ETag != "" {
			req.Header.Set("If-None-Match", stale.Meta.ETag)
		} else if stale.Meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", stale.Meta.LastModified)
		}
	}

	started := time.Now()
	resp, err := o.client.Do(req)
	o.recordTransport(resp, err, time.Since(started))
	if err != nil {
		o.recordFailure(p.name, now)
		if stale != nil && p.staleOnError && staleAllowed(stale.Meta, now) {
			return responseFromEntry(stale, key, "stale"), nil
		}
		return cachedResponse{}, err
	}
	defer resp.Body.Close()
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return cachedResponse{}, fmt.Errorf("upstream content encoding %q is not accepted", encoding)
	}

	if resp.StatusCode == http.StatusNotModified && stale != nil {
		meta := stale.Meta
		freshnessHeaders := make(http.Header)
		for name, value := range meta.Headers {
			freshnessHeaders.Set(name, value)
		}
		for name, value := range safeResponseHeaders(resp.Header) {
			freshnessHeaders.Set(name, value)
		}
		applyFreshness(&meta, freshnessHeaders, p, now)
		if value := resp.Header.Get("ETag"); value != "" {
			meta.ETag = value
		}
		if value := resp.Header.Get("Last-Modified"); value != "" {
			meta.LastModified = value
		}
		for name, value := range safeResponseHeaders(resp.Header) {
			meta.Headers[name] = value
		}
		meta.FetchedAt = now
		meta.LastAccess = now
		if cachePermitted(freshnessHeaders, p.applicationData) {
			_ = o.store.updateMetadata(meta)
		} else {
			if err := o.store.remove(p.scope, key); err != nil {
				return cachedResponse{}, fmt.Errorf("remove prohibited cached response: %w", err)
			}
		}
		o.recordSuccess(p.name, now)
		entry := &cacheEntry{Meta: meta, Body: stale.Body}
		return responseFromEntry(entry, key, "revalidated"), nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBody+1))
	if err != nil {
		o.recordFailure(p.name, now)
		return cachedResponse{}, err
	}
	if int64(len(body)) > p.maxBody {
		return cachedResponse{}, errors.New("upstream response exceeds provider limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if retryableStatus(resp.StatusCode) {
			if resp.StatusCode == http.StatusTooManyRequests && p.group != nil {
				p.group.delay(retryAfter(resp.Header.Get("Retry-After"), now))
			}
			o.recordFailure(p.name, now)
			if stale != nil && p.staleOnError && staleAllowed(stale.Meta, now) {
				return responseFromEntry(stale, key, "stale"), nil
			}
		}
		return cachedResponse{}, &upstreamStatusError{Provider: p.name, Status: resp.StatusCode}
	}
	contentType, err := validateProviderBody(body, resp.Header.Get("Content-Type"), p.contentTypes)
	if err != nil {
		return cachedResponse{}, err
	}
	if request.validate != nil {
		if err := request.validate(body); err != nil {
			return cachedResponse{}, err
		}
	}
	o.recordSuccess(p.name, now)

	meta := cacheMetadata{
		Key: key, Scope: p.scope, Provider: p.name, SourceFingerprint: p.sourceFingerprint,
		Status: resp.StatusCode, Headers: safeResponseHeaders(resp.Header), ContentType: contentType,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"), FetchedAt: now, LastAccess: now,
	}
	applyFreshness(&meta, resp.Header, p, now)
	state := "bypass"
	permitted := cachePermitted(resp.Header, p.applicationData)
	if request.cacheable && permitted {
		admitted, putErr := o.store.putWithAdmission(meta, body, request.admit)
		if putErr == nil {
			state = "miss"
			return cachedResponse{Status: resp.StatusCode, Headers: meta.Headers, Body: body, State: state, Meta: meta, Key: key, AdmittedBytes: admitted}, nil
		}
		if request.admit != nil {
			return cachedResponse{}, fmt.Errorf("cache admission failed: %w", putErr)
		}
	} else if request.cacheable && stale != nil {
		if err := o.store.remove(p.scope, key); err != nil {
			return cachedResponse{}, fmt.Errorf("remove prohibited cached response: %w", err)
		}
	}
	return cachedResponse{Status: resp.StatusCode, Headers: meta.Headers, Body: body, State: state, Meta: meta, Key: key}, nil
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

func validateProviderBody(body []byte, rawContentType string, accepted []string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(rawContentType)
	if err != nil || mediaType == "" {
		return "", errors.New("upstream response has invalid content type")
	}
	allowed := false
	for _, value := range accepted {
		if mediaType == value {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("upstream content type %q is not accepted", mediaType)
	}
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		if !json.Valid(body) {
			return "", errors.New("upstream returned malformed JSON")
		}
	}
	return mediaType, nil
}

func safeResponseHeaders(header http.Header) map[string]string {
	out := make(map[string]string)
	for _, name := range []string{"Cache-Control", "Expires", "ETag", "Last-Modified", "Content-Language"} {
		if value := header.Get(name); value != "" {
			out[name] = value
		}
	}
	return out
}

func hasCacheDirective(value, wanted string) bool {
	for _, directive := range strings.Split(strings.ToLower(value), ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(directive), "=")
		if strings.TrimSpace(name) == wanted {
			return true
		}
	}
	return false
}

func cachePermitted(header http.Header, applicationData bool) bool {
	cacheControl := header.Get("Cache-Control")
	if hasCacheDirective(cacheControl, "no-store") || (!applicationData && hasCacheDirective(cacheControl, "private")) {
		return false
	}
	vary := strings.TrimSpace(header.Get("Vary"))
	if vary == "" {
		return true
	}
	for _, name := range strings.Split(vary, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "accept-language", "accept-encoding":
		default:
			return false
		}
	}
	return true
}

func applyFreshness(meta *cacheMetadata, header http.Header, p *providerPolicy, now time.Time) {
	freshFor, found := cacheMaxAge(header.Get("Cache-Control"))
	if !found {
		if expires, err := http.ParseTime(header.Get("Expires")); err == nil {
			freshFor = max(0, expires.Sub(now))
			found = true
		}
	}
	if !found {
		freshFor = p.fallbackFresh
	}
	if p.maxFresh > 0 && freshFor > p.maxFresh {
		freshFor = p.maxFresh
	}
	meta.FreshUntil = now.Add(freshFor)
	if !p.allowStale || requiresRevalidation(header.Get("Cache-Control")) {
		meta.StaleUntil = meta.FreshUntil
	} else if p.maxStale > 0 {
		meta.StaleUntil = meta.FreshUntil.Add(p.maxStale)
	} else {
		meta.StaleUntil = time.Time{}
	}
	if p.retention > 0 {
		meta.DeleteNoLaterThan = now.Add(p.retention)
		if meta.StaleUntil.IsZero() || meta.StaleUntil.After(meta.DeleteNoLaterThan) {
			meta.StaleUntil = meta.DeleteNoLaterThan
		}
	}
}

func requiresRevalidation(value string) bool {
	for _, raw := range strings.Split(strings.ToLower(value), ",") {
		directive := strings.TrimSpace(raw)
		if directive == "no-cache" || directive == "must-revalidate" || directive == "proxy-revalidate" {
			return true
		}
	}
	return false
}

func cacheMaxAge(value string) (time.Duration, bool) {
	var maxAge, sharedMaxAge time.Duration
	var haveMaxAge, haveSharedMaxAge bool
	for _, raw := range strings.Split(strings.ToLower(value), ",") {
		directive := strings.TrimSpace(raw)
		if directive == "no-cache" {
			return 0, true
		}
		name, seconds, ok := strings.Cut(directive, "=")
		if !ok || (name != "max-age" && name != "s-maxage") {
			continue
		}
		seconds = strings.Trim(seconds, `"`)
		n, err := strconv.ParseInt(seconds, 10, 64)
		if err == nil && n >= 0 {
			if name == "s-maxage" {
				sharedMaxAge, haveSharedMaxAge = time.Duration(n)*time.Second, true
			} else {
				maxAge, haveMaxAge = time.Duration(n)*time.Second, true
			}
		}
	}
	if haveSharedMaxAge {
		return sharedMaxAge, true
	}
	return maxAge, haveMaxAge
}

func (o *outboundClient) circuitOpen(provider string, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	state := o.health[provider]
	return state != nil && now.Before(state.openUntil)
}

func (o *outboundClient) recordFailure(provider string, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	state := o.health[provider]
	if state == nil {
		state = &healthState{}
		o.health[provider] = state
	}
	state.failures++
	if state.failures >= 3 {
		state.openUntil = now.Add(15 * time.Second)
	}
}

func (o *outboundClient) recordSuccess(provider string, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	state := o.health[provider]
	if state == nil {
		state = &healthState{}
		o.health[provider] = state
	}
	state.failures = 0
	state.openUntil = time.Time{}
	state.lastSuccess = now
}

func (o *outboundClient) healthSnapshot() map[string]providerHealth {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	out := make(map[string]providerHealth, len(o.health))
	for name, state := range o.health {
		out[name] = providerHealth{Failures: state.failures, CircuitOpen: now.Before(state.openUntil), LastSuccess: state.lastSuccess}
	}
	return out
}

func (o *outboundClient) recordResult(response cachedResponse, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch response.State {
	case "hit":
		o.stats.CacheHits++
	case "miss":
		o.stats.CacheMisses++
	case "stale":
		o.stats.CacheStale++
	case "revalidated":
		o.stats.CacheRevalidated++
	case "bypass":
		o.stats.CacheBypass++
	}
	var offlineMiss *offlineMissError
	var busy *outboundBusyError
	if errors.As(err, &offlineMiss) {
		o.stats.OfflineMisses++
	}
	if errors.As(err, &busy) {
		o.stats.QueueRejected++
	}
}

func (o *outboundClient) recordTransport(response *http.Response, err error, duration time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stats.NetworkRequests++
	o.stats.Duration += duration
	if duration > o.stats.MaxDuration {
		o.stats.MaxDuration = duration
	}
	if err != nil {
		o.stats.NetworkFailures++
		return
	}
	switch response.StatusCode / 100 {
	case 2:
		o.stats.Status2xx++
	case 3:
		o.stats.Status3xx++
	case 4:
		o.stats.Status4xx++
	case 5:
		o.stats.Status5xx++
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		o.stats.NetworkFailures++
	}
}

func (o *outboundClient) statsSnapshot() outboundStats {
	o.mu.Lock()
	defer o.mu.Unlock()
	stats := o.stats
	stats.InFlight = len(o.inflight)
	return stats
}

type rateGroup struct {
	mu       sync.Mutex
	sem      chan struct{}
	interval time.Duration
	next     time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

func newRateGroup(interval time.Duration) *rateGroup {
	return newConcurrentRateGroup(interval, 1)
}

func newConcurrentRateGroup(interval time.Duration, concurrency int) *rateGroup {
	return &rateGroup{sem: make(chan struct{}, max(1, concurrency)), interval: interval, now: time.Now, sleep: sleepContext}
}

func (g *rateGroup) acquire(ctx context.Context) (func(), error) {
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	g.mu.Lock()
	wait := g.next.Sub(g.now())
	g.mu.Unlock()
	if wait > 0 {
		if err := g.sleep(ctx, wait); err != nil {
			<-g.sem
			return nil, err
		}
	}
	g.mu.Lock()
	g.next = g.now().Add(g.interval)
	g.mu.Unlock()
	return func() { <-g.sem }, nil
}

func (g *rateGroup) delay(until time.Time) {
	if until.IsZero() {
		return
	}
	g.mu.Lock()
	if until.After(g.next) {
		g.next = until
	}
	g.mu.Unlock()
}

func retryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if parsed, err := http.ParseTime(value); err == nil {
		return parsed
	}
	return time.Time{}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func writeCachedResponse(w http.ResponseWriter, response cachedResponse) {
	for name, value := range response.Headers {
		w.Header().Set(name, value)
	}
	if response.Meta.ContentType != "" {
		w.Header().Set("Content-Type", response.Meta.ContentType)
	}
	if response.Meta.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", response.Meta.ContentEncoding)
	}
	setCacheHeaders(w, response)
	w.WriteHeader(response.Status)
	_, _ = w.Write(response.Body)
}

func setCacheHeaders(w http.ResponseWriter, response cachedResponse) {
	w.Header().Set("X-GPX-Cache", response.State)
	if !response.Meta.FetchedAt.IsZero() {
		w.Header().Set("X-GPX-Cached-At", response.Meta.FetchedAt.Format(http.TimeFormat))
		age := max(0, time.Since(response.Meta.FetchedAt).Seconds())
		w.Header().Set("Age", strconv.FormatInt(int64(age), 10))
	}
}

func writeOutboundError(w http.ResponseWriter, err error, _ string) {
	var miss *offlineMissError
	if errors.As(err, &miss) {
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{
			"detail": "This resource is not available in the offline cache",
			"code":   "offline_cache_miss",
			"scope":  miss.Scope,
		})
		return
	}
	var busy *outboundBusyError
	if errors.As(err, &busy) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"detail": "The outbound request queue is full",
			"code":   "outbound_queue_full",
			"scope":  busy.Scope,
		})
		return
	}
	var upstream *upstreamStatusError
	if errors.As(err, &upstream) && upstream.Status >= 400 && upstream.Status < 500 {
		writeError(w, upstream.Status, err.Error())
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}
