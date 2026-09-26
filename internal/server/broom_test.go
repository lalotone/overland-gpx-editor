package server

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
	"github.com/paulmach/orb"
	"google.golang.org/protobuf/encoding/protowire"
)

// broomVersion is reported to upstream providers and in route responses; it
// must follow go.mod rather than the release it was last hand-edited for.
func TestBroomVersionMatchesModule(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info")
	}
	for _, dep := range info.Deps {
		if dep.Path == "code.rbel.co/rubiojr/broom" {
			if dep.Replace != nil {
				t.Skip("broom is replaced locally")
			}
			if dep.Version != "v"+broomVersion {
				t.Fatalf("broomVersion = %q, go.mod requires %s", broomVersion, dep.Version)
			}
			return
		}
	}
	t.Fatal("broom is not a dependency of the test binary")
}

func writeBroomPBF(t *testing.T) (string, []orb.Point) {
	t.Helper()
	message := func(dst []byte, field protowire.Number, value []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(dst, field, protowire.BytesType), value)
	}
	value := func(dst []byte, field protowire.Number, n uint64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(dst, field, protowire.VarintType), n)
	}
	bounds := value(nil, 1, protowire.EncodeZigZag(999_000_000))
	bounds = value(bounds, 2, protowire.EncodeZigZag(1_003_000_000))
	bounds = value(bounds, 3, protowire.EncodeZigZag(42_001_000_000))
	bounds = value(bounds, 4, protowire.EncodeZigZag(41_999_000_000))
	header := message(nil, 1, bounds)
	header = message(header, 4, []byte("OsmSchema-V0.6"))
	stringsTable := []string{"", "highway", "track", "surface", "gravel", "tracktype", "grade2", "motor_vehicle", "yes", "motorcycle"}
	var table []byte
	for _, text := range stringsTable {
		table = message(table, 1, []byte(text))
	}
	points := make([]orb.Point, 120)
	var ids, lons, lats, denseTags []byte
	var previousLon, previousLat int64
	for i := range points {
		points[i] = orb.Point{1 + float64(i)*0.00001, 42}
		lon := int64(math.Round(points[i][0] * 1e7))
		lat := int64(math.Round(points[i][1] * 1e7))
		ids = protowire.AppendVarint(ids, protowire.EncodeZigZag(1))
		lons = protowire.AppendVarint(lons, protowire.EncodeZigZag(lon-previousLon))
		lats = protowire.AppendVarint(lats, protowire.EncodeZigZag(lat-previousLat))
		previousLon, previousLat = lon, lat
		denseTags = append(denseTags, 0)
	}
	dense := message(nil, 1, ids)
	dense = message(dense, 8, lats)
	dense = message(dense, 9, lons)
	dense = message(dense, 10, denseTags)
	group := message(nil, 2, dense)
	for i := 0; i < len(points)-1; i++ {
		way := value(nil, 1, uint64(1000+i))
		way = message(way, 2, []byte{1, 3, 5, 7, 9})
		way = message(way, 3, []byte{2, 4, 6, 8, 8})
		refs := protowire.AppendVarint(nil, protowire.EncodeZigZag(int64(i+1)))
		refs = protowire.AppendVarint(refs, protowire.EncodeZigZag(1))
		way = message(way, 8, refs)
		group = message(group, 3, way)
	}
	data := message(nil, 1, table)
	data = message(data, 2, group)
	var contents []byte
	for i, payload := range [][]byte{header, data} {
		blob := message(nil, 1, payload)
		blockHeader := message(nil, 1, []byte([]string{"OSMHeader", "OSMData"}[i]))
		blockHeader = value(blockHeader, 3, uint64(len(blob)))
		contents = binary.BigEndian.AppendUint32(contents, uint32(len(blockHeader)))
		contents = append(contents, blockHeader...)
		contents = append(contents, blob...)
	}
	path := filepath.Join(t.TempDir(), "route.osm.pbf")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, points
}

func TestBroomProfileVariantsCompile(t *testing.T) {
	profile, warnings, err := broom.ParseProfile(strings.NewReader(broomProfileSource), broomProfileName)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 3 {
		t.Fatalf("profile warnings = %v, want the three documented elevation approximations", warnings)
	}
	for name, overrides := range map[string]map[string]float64{
		"road":  {"overland_surface_bias": 6, "overland_road_bias": 0.05, "offroad_hard_factor": 4},
		"mixed": {"overland_surface_bias": 0.5, "overland_road_bias": 0.6, "offroad_hard_factor": 1},
		"trail": {"overland_surface_bias": 0.1, "overland_road_bias": 1.3, "offroad_hard_factor": 0.4},
	} {
		t.Run(name, func(t *testing.T) {
			resolved, err := profile.With(overrides)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Hash() == profile.Hash() {
				t.Error("profile override did not change its metric identity")
			}
		})
	}
}

func TestRoutingProgressDistinguishesTilesAndRetries(t *testing.T) {
	job := &broomPreparation{}
	job.recordProgress(broom.ProgressEvent{Phase: broom.PhaseElevation, State: broom.StateCompleted, Item: "N41E001.hgt.gz", Done: 100, Total: 100, ItemsDone: 1, ItemsTotal: 2, ItemsReused: 1})
	job.recordProgress(broom.ProgressEvent{Phase: broom.PhaseElevation, State: broom.StateStarted, Item: "N41E002.hgt.gz", Attempt: 1, ItemsDone: 1, ItemsTotal: 2, ItemsReused: 1})
	if job.CompletedItems != 1 || job.Retrying {
		t.Fatalf("next tile: %+v", job)
	}
	job.recordProgress(broom.ProgressEvent{Phase: broom.PhaseElevation, State: broom.StateRetrying, Item: "N41E002.hgt.gz", Attempt: 2, ItemsDone: 1, ItemsTotal: 2, ItemsReused: 1})
	if !job.Retrying || job.CompletedItems != 1 {
		t.Fatalf("retry: %+v", job)
	}
	job.recordProgress(broom.ProgressEvent{Phase: broom.PhaseElevation, State: broom.StateCompleted, Item: "N41E002.hgt.gz", ItemsDone: 2, ItemsTotal: 2, ItemsReused: 1, ItemsDownloaded: 1})
	if job.CompletedItems != 2 {
		t.Fatalf("completed: %+v", job)
	}
}

func TestBroomResponseKeepsAlignedDetails(t *testing.T) {
	route := &broom.Route{
		Geometry:   orb.LineString{{-0.9, 41.6}, {-0.85, 41.65}, {-0.8, 41.7}},
		Elevations: []broom.ElevationSample{{Meters: 100, Valid: true}, {Meters: 110, Valid: true, Interpolated: true}, {}},
		Distance:   1200,
		Duration:   90,
		Legs: []broom.Leg{{Annotations: &broom.Annotations{
			Surface:   []string{"asphalt", "fine_gravel"},
			Tracktype: []string{"", "grade2"},
			Segments: []broom.SegmentAnnotation{
				{GeometryStart: 0, GeometryEnd: 1, Road: broom.RoadAttributes{Highway: "residential"}},
				{GeometryStart: 1, GeometryEnd: 2, Road: broom.RoadAttributes{Highway: "track"}},
			},
		}}},
	}
	response, err := broomResponse(route, "mixed", "spain/aragon", "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Coordinates) != 3 || response.Coordinates[1] != (coordinate{Lat: 41.65, Lon: -0.85}) {
		t.Fatalf("coordinates = %+v", response.Coordinates)
	}
	if response.Elevations[0] == nil || response.Elevations[1] == nil || !response.Elevations[1].Interpolated || response.Elevations[2] != nil {
		t.Fatalf("elevations = %+v", response.Elevations)
	}
	if len(response.Segments) != 2 || response.Segments[1].GeometryStart != 1 || response.Segments[1].GeometryEnd != 2 || response.Segments[1].Surface != "fine_gravel" || response.Segments[1].Tracktype != "grade2" {
		t.Fatalf("segments = %+v", response.Segments)
	}
	if response.DistanceMeters != 1200 || response.DurationSeconds != 90 || response.RegionID != "spain/aragon" || response.GenerationID != "generation-1" {
		t.Fatalf("response metadata = %+v", response)
	}

	route.Elevations = route.Elevations[:2]
	if _, err := broomResponse(route, "mixed", "", ""); err == nil {
		t.Fatal("misaligned elevations were accepted")
	}
	route.Elevations = nil
	route.Legs[0].Annotations.Segments[1].GeometryEnd = 3
	if _, err := broomResponse(route, "mixed", "", ""); err == nil {
		t.Fatal("misaligned annotations were accepted")
	}
}

func TestBroomEndpointsReportUnpreparedData(t *testing.T) {
	s, err := New(Config{GPXDir: t.TempDir(), RoutingCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)

	status := do(t, s, http.MethodGet, "/offline/routing", nil)
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d %s", status.Code, status.Body)
	}
	var decoded broomRoutingStatus
	if err := json.Unmarshal(status.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Enabled || decoded.Ready || decoded.Cached == nil {
		t.Fatalf("status = %+v", decoded)
	}
	config := do(t, s, http.MethodGet, "/config", nil)
	if config.Code != http.StatusOK || !strings.Contains(config.Body.String(), `"broomRoute":"/routing/broom/route"`) || !strings.Contains(config.Body.String(), `"routing":"/offline/routing"`) {
		t.Fatalf("config = %d %s", config.Code, config.Body)
	}

	route := do(t, s, http.MethodPost, "/routing/broom/route", strings.NewReader(`{"waypoints":[{"lat":41.6,"lon":-0.9},{"lat":41.7,"lon":-0.8}],"profile":"mixed"}`))
	if route.Code != http.StatusConflict || !strings.Contains(route.Body.String(), `"code":"routing_data_required"`) {
		t.Fatalf("route = %d %s", route.Code, route.Body)
	}

	invalid := do(t, s, http.MethodPost, "/routing/broom/route", strings.NewReader(`{"waypoints":[{"lat":91,"lon":0},{"lat":41.7,"lon":-0.8}],"profile":"mixed"}`))
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"invalid_routing_request"`) {
		t.Fatalf("invalid route = %d %s", invalid.Code, invalid.Body)
	}
	malformed := do(t, s, http.MethodPost, "/routing/broom/route", strings.NewReader(`{`))
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed route = %d %s", malformed.Code, malformed.Body)
	}
	missingRegion := do(t, s, http.MethodPost, "/offline/routing/prepare", strings.NewReader(`{}`))
	if missingRegion.Code != http.StatusBadRequest {
		t.Fatalf("missing region = %d %s", missingRegion.Code, missingRegion.Body)
	}

	pruned := do(t, s, http.MethodPost, "/offline/routing/prune", strings.NewReader(`{"keepGenerations":1,"removeSources":true,"removeMetrics":true}`))
	if pruned.Code != http.StatusOK {
		t.Fatalf("prune = %d %s", pruned.Code, pruned.Body)
	}
	badPrune := do(t, s, http.MethodPost, "/offline/routing/prune", strings.NewReader(`{"maxBytes":-1}`))
	if badPrune.Code != http.StatusBadRequest {
		t.Fatalf("bad prune = %d %s", badPrune.Code, badPrune.Body)
	}
	missingPin := do(t, s, http.MethodPost, "/offline/routing/pin", strings.NewReader(`{"regionId":"spain/aragon","generationId":"missing","pinned":true}`))
	if missingPin.Code != http.StatusConflict {
		t.Fatalf("missing pin = %d %s", missingPin.Code, missingPin.Body)
	}
}

func TestBroomEndpointRoutesOnBuiltGraph(t *testing.T) {
	pbf, points := writeBroomPBF(t)
	graph := filepath.Join(t.TempDir(), "route.broom")
	if err := broom.Build(t.Context(), pbf, graph, broom.BuildOptions{Jobs: 1}); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{GPXDir: t.TempDir(), RoutingCacheDir: t.TempDir(), RoutingGraph: graph})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	body := `{"waypoints":[{"lat":42,"lon":` + fmt.Sprint(points[1][0]) + `},{"lat":42,"lon":` + fmt.Sprint(points[len(points)-2][0]) + `}],"profile":"mixed"}`
	result := do(t, s, http.MethodPost, "/routing/broom/route", strings.NewReader(body))
	if result.Code != http.StatusOK {
		t.Fatalf("route = %d %s", result.Code, result.Body)
	}
	var response broomRouteResponse
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Coordinates) < 2 || response.DistanceMeters <= 0 || response.DurationSeconds <= 0 {
		t.Fatalf("route = %+v", response)
	}
	if len(response.Segments) == 0 || response.Segments[0].Surface != "gravel" || response.Segments[0].Road.Highway != "track" {
		t.Fatalf("segments = %+v", response.Segments)
	}
	for _, elevation := range response.Elevations {
		if elevation != nil {
			t.Fatalf("graph without DEM invented elevation: %+v", response.Elevations)
		}
	}
	enduroResult := do(t, s, http.MethodPost, "/routing/broom/route", strings.NewReader(strings.Replace(body, `"mixed"`, `"enduro"`, 1)))
	if enduroResult.Code != http.StatusOK {
		t.Fatalf("enduro route = %d %s", enduroResult.Code, enduroResult.Body)
	}
	invalidProfile := do(t, s, http.MethodPost, "/offline/routing/profile", strings.NewReader(`{"source":"---context:way\nassign costfactor = if"}`))
	if invalidProfile.Code != http.StatusBadRequest {
		t.Fatalf("invalid profile accepted: %d %s", invalidProfile.Code, invalidProfile.Body)
	}
	profileBody, _ := json.Marshal(map[string]string{"source": broomProfileSource})
	upload := do(t, s, http.MethodPost, "/offline/routing/profile", strings.NewReader(string(profileBody)))
	if upload.Code != http.StatusCreated {
		t.Fatalf("profile upload = %d %s", upload.Code, upload.Body)
	}
	var uploaded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(upload.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.ID == "" {
		t.Fatal("profile has no session id")
	}
	sessionBody := strings.Replace(body, `"profile":"mixed"`, `"profile":"custom","sessionProfile":"`+uploaded.ID+`"`, 1)
	custom := do(t, s, http.MethodPost, "/routing/broom/route", strings.NewReader(sessionBody))
	if custom.Code != http.StatusOK {
		t.Fatalf("custom route = %d %s", custom.Code, custom.Body)
	}
	dir := s.broom.sessions[uploaded.ID].dataset.temporaryDir
	release := do(t, s, http.MethodPost, "/offline/routing/profile/release", strings.NewReader(`{"id":"`+uploaded.ID+`"}`))
	if release.Code != http.StatusNoContent {
		t.Fatalf("release = %d", release.Code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temporary metrics remain: %v", err)
	}
	custom = do(t, s, http.MethodPost, "/routing/broom/route", strings.NewReader(sessionBody))
	if custom.Code != http.StatusBadRequest {
		t.Fatalf("released profile accepted: %d", custom.Code)
	}
}

func TestEnduroAccessAndPreference(t *testing.T) {
	p, ok := broom.BuiltinProfile("enduro")
	if !ok {
		t.Fatal("Broom Enduro profile is unavailable")
	}
	for _, tt := range []struct {
		name    string
		tags    map[string]string
		allowed bool
	}{
		{"track", map[string]string{"highway": "track", "surface": "gravel"}, true},
		{"unpermitted path", map[string]string{"highway": "path"}, false},
		{"permitted path", map[string]string{"highway": "path", "motorcycle": "yes"}, true},
		{"destination shortcut", map[string]string{"highway": "track", "access": "destination"}, false},
		{"impassable", map[string]string{"highway": "track", "smoothness": "impassable"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := p.EvalWay(tt.tags, false)
			if err != nil {
				t.Fatal(err)
			}
			if (result.CostFactor < 10000) != tt.allowed {
				t.Fatalf("cost %v, allowed %v", result.CostFactor, tt.allowed)
			}
		})
	}
}

func TestBroomManagementRequiresPrivilege(t *testing.T) {
	s, err := New(Config{
		GPXDir: t.TempDir(), RoutingCacheDir: t.TempDir(),
		TrustedUIOrigin: "https://planner.example.test", OfflineAdminToken: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestServer(t, s)
	for _, path := range []string{"/offline/routing/prepare", "/offline/routing/cancel", "/offline/routing/pin", "/offline/routing/prune", "/offline/routing/profile", "/offline/routing/profile/release"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		request.RemoteAddr = "192.0.2.10:1"
		response := httptest.NewRecorder()
		s.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("%s = %d %s", path, response.Code, response.Body)
		}
	}
	trusted := httptest.NewRequest(http.MethodPost, "/offline/routing/prune", strings.NewReader(`{}`))
	trusted.RemoteAddr = "192.0.2.10:1"
	trusted.Header.Set("Origin", "https://planner.example.test")
	trusted.Header.Set("X-GPX-Editor", "1")
	trustedResponse := httptest.NewRecorder()
	s.ServeHTTP(trustedResponse, trusted)
	if trustedResponse.Code != http.StatusOK {
		t.Fatalf("trusted browser = %d %s", trustedResponse.Code, trustedResponse.Body)
	}
	admin := httptest.NewRequest(http.MethodPost, "/offline/routing/prune", strings.NewReader(`{}`))
	admin.RemoteAddr = "192.0.2.10:1"
	admin.Header.Set("Authorization", "Bearer secret")
	adminResponse := httptest.NewRecorder()
	s.ServeHTTP(adminResponse, admin)
	if adminResponse.Code != http.StatusOK {
		t.Fatalf("admin client = %d %s", adminResponse.Code, adminResponse.Body)
	}
}
