package server

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
)

const (
	broomVersion        = "0.5.1"
	broomProfileName    = "overland-motorcycle"
	maxBroomRouteBody   = 64 << 10
	defaultRouteJobs    = 2
	defaultRouteLimit   = 4
	defaultRouteTimeout = 45 * time.Second
)

//go:embed broom_profile.brf
var broomProfileSource string

var errRoutingNotReady = errors.New("local routing data is not ready")
var errRoutingPreparationRunning = errors.New("routing data preparation is already running")

type broomRoutingConfig struct {
	IndexCache       *outboundClient
	CacheDir         string
	Region           string
	Graph            string
	Prepare          bool
	Update           bool
	Jobs             int
	Concurrency      int
	Timeout          time.Duration
	IndexURL         string
	MetadataIndexURL string
	PBFBaseURL       string
	DEMBaseURL       string
	Contact          string
}

type broomProfileSpec struct {
	profile   *broom.Profile
	overrides map[string]float64
}

type broomDataset struct {
	temporaryDir string
	router       *broom.Router
	regionID     string
	generationID string
	name         string
	mu           sync.Mutex
	active       int
	closing      bool
	drained      chan struct{}
}

type broomPreparation struct {
	Diagnostics     []broom.Diagnostic `json:"diagnostics,omitempty"`
	ID              string             `json:"id"`
	RegionID        string             `json:"regionId"`
	State           string             `json:"state"`
	Phase           string             `json:"phase,omitempty"`
	Item            string             `json:"item,omitempty"`
	Done            int64              `json:"done,omitempty"`
	Total           int64              `json:"total,omitempty"`
	Detail          string             `json:"detail,omitempty"`
	StartedAt       string             `json:"startedAt"`
	UpdatedAt       string             `json:"updatedAt"`
	CompletedItems  int                `json:"completedItems"`
	Attempt         int                `json:"attempt,omitempty"`
	Retrying        bool               `json:"retrying,omitempty"`
	ItemsTotal      int64              `json:"itemsTotal,omitempty"`
	ItemsDownloaded int64              `json:"itemsDownloaded,omitempty"`
	ItemsReused     int64              `json:"itemsReused,omitempty"`
	Stage           string             `json:"stage,omitempty"`
	ElapsedSeconds  float64            `json:"elapsedSeconds,omitempty"`
	RetrySeconds    float64            `json:"retrySeconds,omitempty"`
	cancel          context.CancelFunc
}

type broomCachedRegion struct {
	RegionID     string `json:"regionId"`
	Name         string `json:"name"`
	GenerationID string `json:"generationId"`
	Selected     bool   `json:"selected"`
	Pinned       bool   `json:"pinned"`
	InstalledAt  string `json:"installedAt"`
}

type broomRoutingStatus struct {
	Enabled      bool                `json:"enabled"`
	Ready        bool                `json:"ready"`
	RegionID     string              `json:"regionId,omitempty"`
	GenerationID string              `json:"generationId,omitempty"`
	Name         string              `json:"name,omitempty"`
	Error        string              `json:"error,omitempty"`
	Job          *broomPreparation   `json:"job,omitempty"`
	Cached       []broomCachedRegion `json:"cached"`
	CacheBytes   int64               `json:"cacheBytes"`
	PinnedBytes  int64               `json:"pinnedBytes"`
	InUseBytes   int64               `json:"inUseBytes"`
	Reclaimable  int64               `json:"reclaimableBytes"`
}

type broomRoutingService struct {
	sessions      map[string]*sessionBroomProfile
	uploads       chan struct{}
	selectionPath string
	manager       *broom.Manager
	profiles      map[string]broomProfileSpec
	warmup        []*broom.Profile
	modes         *offlineModeController
	ctx           context.Context
	wg            *sync.WaitGroup
	jobs          int
	timeout       time.Duration
	slots         chan struct{}

	mu      sync.RWMutex
	current *broomDataset
	job     *broomPreparation
	lastErr string
}

type modeBoundTransport struct {
	base  http.RoundTripper
	modes *offlineModeController
}

type modeBoundBody struct {
	io.ReadCloser
	done sync.Once
	end  func()
}

func (b *modeBoundBody) Close() error {
	err := b.ReadCloser.Close()
	b.done.Do(b.end)
	return err
}

func (t modeBoundTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, done, ok := t.modes.networkContext(request.Context())
	if !ok {
		return nil, &offlineMissError{Scope: "routing-data"}
	}
	response, err := t.base.RoundTrip(request.Clone(ctx))
	if err != nil {
		done()
		return nil, err
	}
	if response.Body == nil {
		response.Body = http.NoBody
		done()
		return response, nil
	}
	response.Body = &modeBoundBody{ReadCloser: response.Body, end: done}
	return response, nil
}

func newBroomRoutingService(ctx context.Context, wg *sync.WaitGroup, modes *offlineModeController, cfg broomRoutingConfig, client *http.Client) (*broomRoutingService, error) {
	profile, warnings, err := broom.ParseProfile(strings.NewReader(broomProfileSource), broomProfileName)
	if err != nil {
		return nil, fmt.Errorf("compile routing profile: %w", err)
	}
	for _, warning := range warnings {
		slog.Warn("Broom profile compatibility warning", "warning", warning)
	}
	definitions := map[string]map[string]float64{
		"road":  {"overland_surface_bias": 6, "overland_road_bias": 0.05, "offroad_hard_factor": 4},
		"mixed": {"overland_surface_bias": 0.5, "overland_road_bias": 0.6, "offroad_hard_factor": 1},
		"trail": {"overland_surface_bias": 0.1, "overland_road_bias": 1.3, "offroad_hard_factor": 0.4},
	}
	profiles := make(map[string]broomProfileSpec, len(definitions))
	warmup := make([]*broom.Profile, 0, len(definitions))
	for name, overrides := range definitions {
		resolved, resolveErr := profile.With(overrides)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve %s routing profile: %w", name, resolveErr)
		}
		profiles[name] = broomProfileSpec{profile: resolved, overrides: overrides}
		warmup = append(warmup, resolved)
	}
	enduro, ok := broom.BuiltinProfile("enduro")
	if !ok {
		return nil, errors.New("built-in Enduro profile is unavailable")
	}
	profiles["enduro"] = broomProfileSpec{profile: enduro}
	warmup = append(warmup, enduro)
	registered := []*broom.Profile{profile, enduro}
	slices.SortFunc(warmup, func(a, b *broom.Profile) int { return strings.Compare(a.Hash(), b.Hash()) })
	if cfg.Jobs <= 0 {
		cfg.Jobs = defaultRouteJobs
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultRouteLimit
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultRouteTimeout
	}
	httpClient := *client
	httpClient.Timeout = 0
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	httpClient.Transport = modeBoundTransport{base: base, modes: modes}
	if cfg.IndexCache != nil {
		indexURL, err := url.Parse(valueOrDefault(cfg.IndexURL, "https://download.geofabrik.de/index-v1.json"))
		if err != nil {
			return nil, err
		}
		httpClient.Transport = broomIndexTransport{base: httpClient.Transport, outbound: cfg.IndexCache, policy: newProviderPolicy("routing-index", "routing-index", indexURL, 24*time.Hour, 30*24*time.Hour, 90*24*time.Hour, true, 32<<20, []string{"application/json", "application/geo+json"}, newRateGroup(0), true)}
	}
	zeroDistance := 0.0
	manager, err := broom.New(broom.Options{
		CacheDir:   cfg.CacheDir,
		HTTPClient: &httpClient,
		Provider: broom.ProviderOptions{
			IndexURL: cfg.IndexURL, MetadataIndexURL: cfg.MetadataIndexURL,
			PBFBaseURL: cfg.PBFBaseURL, DEMBaseURL: cfg.DEMBaseURL,
			UserAgent: "overland-gpx-editor/1 broom/" + broomVersion, Contact: cfg.Contact,
		},
		Profiles: registered,
		Router: broom.RouterOptions{
			Profiles: registered, MetricCacheSize: len(profiles),
			DisableCustomizeOnDemand: true, MaxUncustomizedDistance: &zeroDistance,
		},
	})
	if err != nil {
		return nil, err
	}
	service := &broomRoutingService{
		sessions: make(map[string]*sessionBroomProfile), uploads: make(chan struct{}, 1),
		selectionPath: filepath.Join(cfg.CacheDir, "overland-active-region"),
		manager:       manager, profiles: profiles, warmup: warmup, modes: modes,
		ctx: ctx, wg: wg, jobs: cfg.Jobs, timeout: cfg.Timeout,
		slots: make(chan struct{}, cfg.Concurrency),
	}
	if cfg.Graph != "" {
		router, openErr := broom.Open(cfg.Graph, broom.RouterOptions{
			Profiles: registered, MetricCacheSize: len(profiles),
			DisableCustomizeOnDemand: true, MaxUncustomizedDistance: &zeroDistance,
		})
		if openErr != nil {
			return nil, fmt.Errorf("open routing graph: %w", openErr)
		}
		if warmErr := service.warmRouter(ctx, router); warmErr != nil {
			_ = router.Close()
			return nil, fmt.Errorf("warm routing graph: %w", warmErr)
		}
		service.current = newBroomDataset(router, "", "", "Local routing graph")
		return service, nil
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		if saved, err := os.ReadFile(service.selectionPath); err == nil {
			region = strings.TrimSpace(string(saved))
		}
	}
	if region == "" {
		region = service.latestSelectedRegion(ctx)
	}
	if region == "" {
		return service, nil
	}
	if cfg.Prepare || cfg.Update {
		if startErr := service.startPreparation(region, cfg.Update); startErr != nil {
			service.lastErr = startErr.Error()
		}
		return service, nil
	}
	ready, openErr := manager.OpenRegion(ctx, region, broom.OpenRegionOptions{})
	if openErr != nil {
		service.lastErr = openErr.Error()
		return service, nil
	}
	if warmErr := service.warmRouter(ctx, ready.Router); warmErr != nil {
		_ = ready.Router.Close()
		service.lastErr = warmErr.Error()
		return service, nil
	}
	service.current = datasetFromSetup(ready)
	return service, nil
}

func (s *broomRoutingService) latestSelectedRegion(ctx context.Context) string {
	regions, err := s.manager.CachedRegions(ctx)
	if err != nil {
		return ""
	}
	selected := ""
	var latest time.Time
	for _, region := range regions {
		if region.Selected && !region.Slim && region.Elevation == broom.Auto {
			if selected == "" || region.InstalledAt.After(latest) {
				selected, latest = region.RegionID, region.InstalledAt
			}
		}
	}
	return selected
}

func datasetFromSetup(ready broom.SetupResult) *broomDataset {
	return newBroomDataset(ready.Router, ready.Generation.RegionID, ready.Generation.GenerationID, ready.Generation.Name)
}

func newBroomDataset(router *broom.Router, regionID, generationID, name string) *broomDataset {
	return &broomDataset{router: router, regionID: regionID, generationID: generationID, name: name, drained: make(chan struct{})}
}

func (d *broomDataset) acquire() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return false
	}
	d.active++
	return true
}

func (d *broomDataset) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active--
	if d.closing && d.active == 0 {
		close(d.drained)
	}
}

func (d *broomDataset) close() error {
	d.mu.Lock()
	d.closing = true
	if d.active == 0 {
		close(d.drained)
	}
	d.mu.Unlock()
	<-d.drained
	err := d.router.Close()
	if d.temporaryDir != "" {
		err = errors.Join(err, os.RemoveAll(d.temporaryDir))
	}
	return err
}

func (s *broomRoutingService) warmRouter(ctx context.Context, router *broom.Router) error {
	for _, profile := range s.warmup {
		if err := router.Customize(ctx, profile); err != nil {
			return err
		}
	}
	return nil
}

func (s *broomRoutingService) replaceDataset(next *broomDataset) {
	if next.regionID != "" && s.selectionPath != "" {
		if err := saveActiveRoutingRegion(s.selectionPath, next.regionID); err != nil {
			slog.Warn("persist active routing region", "error", err)
		}
	}
	s.mu.Lock()
	previous := s.current
	s.current = next
	s.lastErr = ""
	s.mu.Unlock()
	if previous != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := previous.close(); err != nil {
				slog.Warn("close superseded Broom graph", "error", err)
			}
		}()
	}
}

func saveActiveRoutingRegion(filename, region string) error {
	file, err := os.CreateTemp(filepath.Dir(filename), ".active-region-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(region); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filename)
}

func (s *broomRoutingService) startPreparation(region string, update bool) error {
	region = strings.TrimSpace(region)
	if region == "" || len(region) > 200 {
		return errors.New("routing region must contain 1..200 characters")
	}
	if s.modes.mode() == modeCacheOnly {
		ready, err := s.manager.OpenRegion(s.ctx, region, broom.OpenRegionOptions{})
		if err != nil {
			return fmt.Errorf("routing data is not installed and cannot be acquired in cache-only mode: %w", err)
		}
		if err := s.warmRouter(s.ctx, ready.Router); err != nil {
			_ = ready.Router.Close()
			return err
		}
		s.replaceDataset(datasetFromSetup(ready))
		return nil
	}
	s.mu.Lock()
	if s.job != nil && (s.job.State == "queued" || s.job.State == "running") {
		s.mu.Unlock()
		return errRoutingPreparationRunning
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	job := &broomPreparation{
		ID: fmt.Sprintf("routing-%d", time.Now().UnixNano()), RegionID: region,
		State: "queued", StartedAt: now, UpdatedAt: now, cancel: cancel,
	}
	s.job = job
	s.lastErr = ""
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runPreparation(jobCtx, job, update)
	}()
	return nil
}

func (s *broomRoutingService) runPreparation(ctx context.Context, job *broomPreparation, update bool) {
	s.updateJob(job, func(j *broomPreparation) { j.State = "running" })
	ready, err := s.manager.EnsureRegion(ctx, job.RegionID, broom.EnsureOptions{
		Update: update,
		Setup: broom.SetupOptions{Jobs: s.jobs, Elevation: broom.Auto, WarmupProfiles: s.warmup, Progress: func(event broom.ProgressEvent) {
			s.updateJob(job, func(j *broomPreparation) { j.recordProgress(event) })
		}, Diagnostic: func(diagnostic broom.Diagnostic) {
			s.updateJob(job, func(j *broomPreparation) {
				if len(j.Diagnostics) < 32 {
					j.Diagnostics = append(slices.Clone(j.Diagnostics), diagnostic)
				}
			})
		}},
	})
	if err != nil {
		state := "failed"
		if errors.Is(err, context.Canceled) {
			state = "cancelled"
		}
		s.updateJob(job, func(j *broomPreparation) { j.State, j.Detail = state, err.Error() })
		s.mu.Lock()
		s.lastErr = err.Error()
		s.mu.Unlock()
		return
	}
	if ctx.Err() != nil {
		_ = ready.Router.Close()
		s.updateJob(job, func(j *broomPreparation) { j.State, j.Detail = "cancelled", ctx.Err().Error() })
		return
	}
	s.replaceDataset(datasetFromSetup(ready))
	s.updateJob(job, func(j *broomPreparation) {
		j.State, j.Phase, j.Detail = "complete", "", ""
		j.Done, j.Total = 0, 0
	})
}

func (j *broomPreparation) recordProgress(event broom.ProgressEvent) {
	j.Phase, j.Item = string(event.Phase), valueOrDefault(event.ItemLabel, path.Base(event.Item))
	j.CompletedItems, j.ItemsTotal = int(event.ItemsDone), event.ItemsTotal
	j.ItemsDownloaded, j.ItemsReused = event.ItemsDownloaded, event.ItemsReused
	j.Stage, j.ElapsedSeconds, j.RetrySeconds = event.Stage, event.Elapsed.Seconds(), event.RetryIn.Seconds()
	j.Done, j.Total = event.Done, event.Total
	j.Attempt, j.Retrying = event.Attempt, event.State == broom.StateRetrying
}

func (s *broomRoutingService) updateJob(target *broomPreparation, update func(*broomPreparation)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != target {
		return
	}
	update(target)
	target.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
}

func (s *broomRoutingService) cancelPreparation() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil || (s.job.State != "queued" && s.job.State != "running") {
		return false
	}
	s.job.cancel()
	return true
}

func (s *broomRoutingService) status(ctx context.Context) broomRoutingStatus {
	s.mu.RLock()
	status := broomRoutingStatus{Enabled: true, Error: s.lastErr, Cached: []broomCachedRegion{}}
	if s.current != nil {
		status.Ready = true
		status.RegionID, status.GenerationID, status.Name = s.current.regionID, s.current.generationID, s.current.name
	}
	if s.job != nil {
		copy := *s.job
		copy.cancel = nil
		status.Job = &copy
	}
	s.mu.RUnlock()
	regions, err := s.manager.CachedRegions(ctx)
	if err == nil {
		for _, region := range regions {
			status.Cached = append(status.Cached, broomCachedRegion{
				RegionID: region.RegionID, Name: region.Name, GenerationID: region.GenerationID,
				Selected: region.Selected, Pinned: region.Pinned, InstalledAt: region.InstalledAt.UTC().Format(time.RFC3339),
			})
		}
	}
	if inventory, inventoryErr := s.manager.CacheInfo(ctx); inventoryErr == nil {
		status.CacheBytes = inventory.Bytes
		status.PinnedBytes = inventory.PinnedBytes
		status.InUseBytes = inventory.InUseBytes
		status.Reclaimable = inventory.ReclaimableBytes
	}
	return status
}

func (s *broomRoutingService) close() error {
	s.mu.Lock()
	if s.job != nil {
		s.job.cancel()
	}
	current := s.current
	s.current = nil
	s.mu.Unlock()
	if current != nil {
		return current.close()
	}
	return nil
}

type requiredCoordinate struct {
	Lat *float64 `json:"lat"`
	Lon *float64 `json:"lon"`
}

func (c requiredCoordinate) coordinate() (coordinate, error) {
	if c.Lat == nil || c.Lon == nil {
		return coordinate{}, errors.New("every waypoint requires lat and lon")
	}
	point := coordinate{Lat: *c.Lat, Lon: *c.Lon}
	if !point.valid() {
		return coordinate{}, errors.New("waypoint coordinates are out of range")
	}
	return point, nil
}

type broomRouteRequest struct {
	SessionProfile string               `json:"sessionProfile,omitempty"`
	Waypoints      []requiredCoordinate `json:"waypoints"`
	Profile        string               `json:"profile"`
}

type broomElevationSample struct {
	Meters       float64 `json:"meters"`
	Interpolated bool    `json:"interpolated"`
}

type broomRouteSegment struct {
	GeometryStart int                  `json:"geometryStart"`
	GeometryEnd   int                  `json:"geometryEnd"`
	Surface       string               `json:"surface,omitempty"`
	Tracktype     string               `json:"tracktype,omitempty"`
	Road          broom.RoadAttributes `json:"road"`
	Beeline       bool                 `json:"beeline,omitempty"`
}

type broomRouteResponse struct {
	SchemaVersion   int                     `json:"schemaVersion"`
	Engine          string                  `json:"engine"`
	EngineVersion   string                  `json:"engineVersion"`
	Profile         string                  `json:"profile"`
	RegionID        string                  `json:"regionId,omitempty"`
	GenerationID    string                  `json:"generationId,omitempty"`
	Coordinates     []coordinate            `json:"coordinates"`
	Elevations      []*broomElevationSample `json:"elevations"`
	Segments        []broomRouteSegment     `json:"segments"`
	DistanceMeters  float64                 `json:"distanceMeters"`
	DurationSeconds float64                 `json:"durationSeconds"`
}

func (s *broomRoutingService) route(ctx context.Context, request broomRouteRequest) (broomRouteResponse, error) {
	if len(request.Waypoints) < 2 || len(request.Waypoints) > 100 {
		return broomRouteResponse{}, errors.New("waypoints must contain 2..100 coordinates")
	}
	spec, ok := s.profiles[request.Profile]
	if !ok && request.Profile != "custom" {
		return broomRouteResponse{}, errors.New("profile must be road, mixed, trail, enduro, or custom")
	}
	waypoints := make([]broom.Waypoint, len(request.Waypoints))
	for i, raw := range request.Waypoints {
		point, err := raw.coordinate()
		if err != nil {
			return broomRouteResponse{}, err
		}
		waypoints[i] = broom.Waypoint{Point: broom.Point{Lon: point.Lon, Lat: point.Lat}, Type: broom.WaypointBreak}
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return broomRouteResponse{}, &routingBusyError{}
	}
	queryCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	stopRootCancel := context.AfterFunc(s.ctx, cancel)
	defer stopRootCancel()
	s.mu.RLock()
	dataset := s.current
	if request.Profile == "custom" {
		session := s.sessions[request.SessionProfile]
		if session == nil || time.Now().After(session.expires) || dataset == nil || session.graph != dataset.router.Path() {
			s.mu.RUnlock()
			return broomRouteResponse{}, errors.New("session profile expired or routing region changed; upload the BRF again")
		}
		dataset, spec = session.dataset, broomProfileSpec{profile: session.profile}
	}
	if dataset == nil || !dataset.acquire() {
		s.mu.RUnlock()
		return broomRouteResponse{}, errRoutingNotReady
	}
	s.mu.RUnlock()
	defer dataset.release()
	route, err := dataset.router.Route(queryCtx, broom.Request{
		Profile: spec.profile.Name, Overrides: spec.overrides, Waypoints: waypoints,
		Geometry: broom.GeometryFull, Annotations: true, Elevations: true,
	})
	if err != nil {
		return broomRouteResponse{}, err
	}
	if len(route.Geometry) < 2 {
		return broomRouteResponse{}, errors.New("routing returned an empty geometry")
	}
	return broomResponse(route, request.Profile, dataset.regionID, dataset.generationID)
}

func broomResponse(route *broom.Route, profile, regionID, generationID string) (broomRouteResponse, error) {
	if len(route.Elevations) != 0 && len(route.Elevations) != len(route.Geometry) {
		return broomRouteResponse{}, errors.New("routing returned misaligned elevations")
	}
	response := broomRouteResponse{
		SchemaVersion: 1, Engine: "Broom", EngineVersion: broomVersion, Profile: profile,
		RegionID: regionID, GenerationID: generationID,
		Coordinates: make([]coordinate, len(route.Geometry)), Elevations: make([]*broomElevationSample, len(route.Geometry)),
		DistanceMeters: route.Distance, DurationSeconds: route.Duration,
	}
	for i, point := range route.Geometry {
		response.Coordinates[i] = coordinate{Lat: point[1], Lon: point[0]}
		if len(route.Elevations) != 0 && route.Elevations[i].Valid {
			response.Elevations[i] = &broomElevationSample{Meters: route.Elevations[i].Meters, Interpolated: route.Elevations[i].Interpolated}
		}
	}
	for _, leg := range route.Legs {
		if leg.Annotations == nil {
			continue
		}
		for i, segment := range leg.Annotations.Segments {
			if segment.GeometryStart < 0 || segment.GeometryEnd < segment.GeometryStart || segment.GeometryEnd >= len(route.Geometry) {
				return broomRouteResponse{}, errors.New("routing returned misaligned annotations")
			}
			item := broomRouteSegment{GeometryStart: segment.GeometryStart, GeometryEnd: segment.GeometryEnd, Road: segment.Road, Beeline: segment.Beeline}
			if i < len(leg.Annotations.Surface) {
				item.Surface = leg.Annotations.Surface[i]
			}
			if i < len(leg.Annotations.Tracktype) {
				item.Tracktype = leg.Annotations.Tracktype[i]
			}
			response.Segments = append(response.Segments, item)
		}
	}
	return response, nil
}

type routingBusyError struct{}

func (*routingBusyError) Error() string { return "routing query queue is full" }

func writeBroomError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "routing_failed"
	switch {
	case errors.Is(err, errRoutingNotReady), errors.Is(err, broom.ErrRegionNotInstalled):
		status, code = http.StatusConflict, "routing_data_required"
	case errors.Is(err, errRoutingPreparationRunning):
		status, code = http.StatusConflict, "routing_preparation_running"
	case errors.Is(err, broom.ErrMetricNotCustomized):
		status, code = http.StatusConflict, "routing_warmup_required"
	case errors.Is(err, broom.ErrNoRoute), errors.Is(err, broom.ErrNoSegment):
		status, code = http.StatusUnprocessableEntity, "no_route"
	case errors.Is(err, broom.ErrCatalogue), errors.Is(err, broom.ErrIncompatibleGeneration):
		status, code = http.StatusServiceUnavailable, "routing_data_invalid"
	case errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusGatewayTimeout, "routing_timeout"
	case errors.Is(err, context.Canceled):
		status, code = http.StatusRequestTimeout, "routing_cancelled"
	default:
		var busy *routingBusyError
		if errors.As(err, &busy) {
			status, code = http.StatusTooManyRequests, "routing_busy"
		} else if strings.Contains(err.Error(), "waypoint") || strings.Contains(err.Error(), "profile") {
			status, code = http.StatusBadRequest, "invalid_routing_request"
		}
	}
	writeJSON(w, status, map[string]string{"detail": err.Error(), "code": code, "scope": "routing"})
}

func (s *Server) handleBroomRoute(w http.ResponseWriter, r *http.Request) {
	if s.broom == nil {
		writeError(w, http.StatusNotFound, "Local routing is disabled")
		return
	}
	var request broomRouteRequest
	if err := decodeJSONBody(w, r, maxBroomRouteBody, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	response, err := s.broom.route(r.Context(), request)
	if err != nil {
		writeBroomError(w, err)
		return
	}
	noStoreJSON(w, http.StatusOK, response)
}

func (s *Server) handleBroomStatus(w http.ResponseWriter, r *http.Request) {
	if s.broom == nil {
		writeError(w, http.StatusNotFound, "Local routing is disabled")
		return
	}
	noStoreJSON(w, http.StatusOK, s.broom.status(r.Context()))
}

func (s *Server) handleBroomPrepare(w http.ResponseWriter, r *http.Request) {
	if s.broom == nil {
		writeError(w, http.StatusNotFound, "Local routing is disabled")
		return
	}
	var request struct {
		RegionID string `json:"regionId"`
		Update   bool   `json:"update"`
	}
	if err := decodeJSONBody(w, r, 4<<10, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(request.RegionID) == "" || len(strings.TrimSpace(request.RegionID)) > 200 {
		writeError(w, http.StatusBadRequest, "routing region must contain 1..200 characters")
		return
	}
	if err := s.broom.startPreparation(request.RegionID, request.Update); err != nil {
		writeBroomError(w, err)
		return
	}
	noStoreJSON(w, http.StatusAccepted, s.broom.status(r.Context()))
}

func (s *Server) handleBroomCancel(w http.ResponseWriter, r *http.Request) {
	if s.broom == nil {
		writeError(w, http.StatusNotFound, "Local routing is disabled")
		return
	}
	if !s.broom.cancelPreparation() {
		writeError(w, http.StatusConflict, "No routing data preparation is running")
		return
	}
	noStoreJSON(w, http.StatusAccepted, s.broom.status(r.Context()))
}

func (s *Server) handleBroomPin(w http.ResponseWriter, r *http.Request) {
	if s.broom == nil {
		writeError(w, http.StatusNotFound, "Local routing is disabled")
		return
	}
	var request struct {
		RegionID     string `json:"regionId"`
		GenerationID string `json:"generationId"`
		Pinned       bool   `json:"pinned"`
	}
	if err := decodeJSONBody(w, r, 4<<10, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.RegionID = strings.TrimSpace(request.RegionID)
	request.GenerationID = strings.TrimSpace(request.GenerationID)
	if request.RegionID == "" || request.GenerationID == "" || len(request.RegionID) > 200 || len(request.GenerationID) > 200 {
		writeError(w, http.StatusBadRequest, "regionId and generationId are required")
		return
	}
	if err := s.broom.manager.PinRegion(r.Context(), request.RegionID, request.GenerationID, request.Pinned); err != nil {
		writeBroomError(w, err)
		return
	}
	noStoreJSON(w, http.StatusOK, s.broom.status(r.Context()))
}

func (s *Server) handleBroomPrune(w http.ResponseWriter, r *http.Request) {
	if s.broom == nil {
		writeError(w, http.StatusNotFound, "Local routing is disabled")
		return
	}
	var request struct {
		DryRun          bool   `json:"dryRun"`
		MaxBytes        *int64 `json:"maxBytes"`
		OlderThan       string `json:"olderThan"`
		KeepGenerations int    `json:"keepGenerations"`
		RemoveSources   bool   `json:"removeSources"`
		RemoveMetrics   bool   `json:"removeMetrics"`
	}
	if err := decodeJSONBody(w, r, 4<<10, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.KeepGenerations < 0 || request.MaxBytes != nil && *request.MaxBytes < 0 {
		writeError(w, http.StatusBadRequest, "cache limits must not be negative")
		return
	}
	var olderThan time.Time
	if request.OlderThan != "" {
		var err error
		olderThan, err = time.Parse(time.RFC3339, request.OlderThan)
		if err != nil {
			writeError(w, http.StatusBadRequest, "olderThan must be an RFC3339 timestamp")
			return
		}
	}
	if _, err := s.broom.manager.Prune(r.Context(), broom.PruneOptions{
		DryRun: request.DryRun, MaxBytes: request.MaxBytes, OlderThan: olderThan,
		KeepGenerations: request.KeepGenerations, RemoveSources: request.RemoveSources,
		RemoveMetrics: request.RemoveMetrics,
	}); err != nil {
		writeBroomError(w, err)
		return
	}
	noStoreJSON(w, http.StatusOK, s.broom.status(r.Context()))
}
