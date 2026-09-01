package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	maxPackResources        = 10000
	maxActivePackJobs       = 2
	maxStoredPackManifests  = 16
	maxPackManifestBytes    = 1 << 20
	maxMapGenerationBytes   = 4 << 20
	maxElevationPackEntries = 2048
	maxElevationPackBytes   = 512 << 20
	defaultPackJobTimeout   = 30 * time.Minute

	packResourceVectorMap    = "vector-map"
	packResourceElevation    = "elevation"
	packResourceFuelStations = "fuel-stations"
	packResourceWater        = "water"
	packResourceCampsites    = "campsites"
	packResourceFuelPrices   = "fuel-prices"
	packResourceRoute        = "route"
	packResourceSurface      = "surface"
	packResourcePlaces       = "places"
	packResourcePOIs         = "points-of-interest"
)

type packInput struct {
	Name      string       `json:"name"`
	Automatic bool         `json:"automatic,omitempty"`
	BBox      *bbox        `json:"bbox,omitempty"`
	Route     []coordinate `json:"route,omitempty"`
	PaddingKM float64      `json:"paddingKm"`
	ZoomMin   int          `json:"zoomMin"`
	ZoomMax   int          `json:"zoomMax"`
	Layers    []string     `json:"layers,omitempty"`
	Scopes    []string     `json:"scopes,omitempty"`
}

func (p *packInput) UnmarshalJSON(raw []byte) error {
	type wireInput struct {
		Name      string          `json:"name"`
		Automatic bool            `json:"automatic"`
		BBox      json.RawMessage `json:"bbox"`
		Route     []coordinate    `json:"route"`
		PaddingKM float64         `json:"paddingKm"`
		ZoomMin   *int            `json:"zoomMin"`
		ZoomMax   *int            `json:"zoomMax"`
		MinZoom   *int            `json:"minZoom"`
		MaxZoom   *int            `json:"maxZoom"`
		Layers    []string        `json:"layers"`
		Scopes    []string        `json:"scopes"`
	}
	var wire wireInput
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	p.Name, p.Automatic, p.Route, p.PaddingKM, p.Layers, p.Scopes = wire.Name, wire.Automatic, wire.Route, wire.PaddingKM, wire.Layers, wire.Scopes
	if wire.ZoomMin != nil {
		p.ZoomMin = *wire.ZoomMin
	} else if wire.MinZoom != nil {
		p.ZoomMin = *wire.MinZoom
	}
	if wire.ZoomMax != nil {
		p.ZoomMax = *wire.ZoomMax
	} else if wire.MaxZoom != nil {
		p.ZoomMax = *wire.MaxZoom
	}
	if len(wire.BBox) != 0 && string(wire.BBox) != "null" {
		var object bbox
		if err := json.Unmarshal(wire.BBox, &object); err == nil {
			p.BBox = &object
			return nil
		}
		var tuple []float64
		if err := json.Unmarshal(wire.BBox, &tuple); err != nil || len(tuple) != 4 {
			return errors.New("bbox must be an object or [south, west, north, east]")
		}
		p.BBox = &bbox{South: tuple[0], West: tuple[1], North: tuple[2], East: tuple[3]}
	}
	return nil
}

type blockedPackResource struct {
	Layer    string `json:"layer,omitempty"`
	Resource string `json:"resource,omitempty"`
	Reason   string `json:"reason"`
}

type packEstimate struct {
	Resources      int                       `json:"resources"`
	EstimatedBytes int64                     `json:"estimatedBytes"`
	Reused         int                       `json:"reused"`
	ReusedBytes    int64                     `json:"reusedBytes"`
	RemainingQuota int64                     `json:"remainingQuota"`
	FinalBytes     int64                     `json:"finalBytes"`
	Counts         map[string]int            `json:"counts"`
	Scopes         map[string]cacheScopeStat `json:"scopes"`
	Blocked        []blockedPackResource     `json:"blocked,omitempty"`
	Dynamic        []string                  `json:"dynamic,omitempty"`
	Detail         string                    `json:"detail,omitempty"`
	Bounds         bbox                      `json:"-"`
	ElevationTiles []tileKey                 `json:"-"`
	MapTiles       map[string][]tileKey      `json:"-"`
	RasterMapTiles map[string][]tileKey      `json:"-"`
	ExistingKeys   []existingPackResource    `json:"-"`
	GenericBytes   int64                     `json:"-"`
	Glyphs         []glyphPackResource       `json:"-"`
	POIRequests    []poiPackResource         `json:"-"`
}

type glyphPackResource struct {
	Font  string
	Range string
}

type existingPackResource struct {
	Category string
	Key      string
}

type poiPackResource struct {
	Category string
	Request  cachedRequest
}

type packResourceProgress struct {
	Done   int   `json:"done"`
	Total  int   `json:"total"`
	Failed int   `json:"failed"`
	Bytes  int64 `json:"bytes"`
	Items  int   `json:"items,omitempty"`
}

type packManifest struct {
	ID        string                          `json:"id"`
	Name      string                          `json:"name"`
	State     string                          `json:"state"`
	CreatedAt time.Time                       `json:"createdAt"`
	UpdatedAt time.Time                       `json:"updatedAt"`
	Done      int                             `json:"done"`
	Total     int                             `json:"total"`
	Bytes     int64                           `json:"bytes"`
	Failures  int                             `json:"failures"`
	Resources map[string]packResourceProgress `json:"resources,omitempty"`
	Input     packInput                       `json:"input"`
	CacheKeys []string                        `json:"cacheKeys,omitempty"`
	ErrorCode string                          `json:"errorCode,omitempty"`

	cancel  context.CancelFunc `json:"-"`
	deleted bool               `json:"-"`
}

type packSummary struct {
	ID        string                          `json:"id"`
	Name      string                          `json:"name"`
	State     string                          `json:"state"`
	BBox      *bbox                           `json:"bbox,omitempty"`
	Done      int                             `json:"done"`
	Total     int                             `json:"total"`
	Failures  int                             `json:"failed"`
	Bytes     int64                           `json:"bytes"`
	Resources map[string]packResourceProgress `json:"resources,omitempty"`
	ErrorCode string                          `json:"errorCode,omitempty"`
	Error     string                          `json:"error,omitempty"`
	CreatedAt time.Time                       `json:"createdAt"`
	UpdatedAt time.Time                       `json:"updatedAt"`
}

type packManager struct {
	server             *Server
	mu                 sync.Mutex
	packs              map[string]*packManifest
	jobTimeout         time.Duration
	replacementCleanup func(*packManifest) error
}

type packBudgetError struct{}

func (packBudgetError) Error() string { return "pack exceeded its actual cache byte budget" }

type packAdmissionBudget struct {
	mu    sync.Mutex
	limit int64
	used  int64
}

func (b *packAdmissionBudget) admit(bytes int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes < 0 || bytes > b.limit-b.used {
		return packBudgetError{}
	}
	b.used += bytes
	return nil
}

func newPackManager(server *Server) (*packManager, error) {
	m := &packManager{server: server, packs: make(map[string]*packManifest), jobTimeout: defaultPackJobTimeout}
	if !server.cache.writable {
		return m, nil
	}
	changed, err := m.loadStoredPacks()
	if err != nil {
		return nil, err
	}
	if err := m.pruneExcessStoredPacks(changed); err != nil {
		return nil, err
	}
	desired, unavailable := m.desiredStoredPins()
	if server.openFreeMap != nil {
		server.openFreeMap.addDesiredPins(desired)
	}
	reconcileUnavailable, _, err := server.cache.reconcilePins(desired)
	if err != nil {
		return nil, fmt.Errorf("reconcile cache pins: %w", err)
	}
	for key := range reconcileUnavailable {
		unavailable[key] = struct{}{}
	}
	if err := m.persistRecoveredPacks(changed, unavailable); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *packManager) loadStoredPacks() (map[string]bool, error) {
	entries, err := fs.ReadDir(m.server.cache.root.FS(), filepath.ToSlash(m.server.cache.packsRel))
	if err != nil {
		return nil, err
	}
	changed := make(map[string]bool)
	for _, entry := range entries {
		filenameID := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !validPackID(filenameID) {
			continue
		}
		raw, err := readFileLimitAt(m.server.cache.root, filepath.Join(m.server.cache.packsRel, entry.Name()), maxPackManifestBytes)
		if err != nil {
			continue
		}
		var manifest packManifest
		if json.Unmarshal(raw, &manifest) != nil || manifest.ID != filenameID || len(manifest.CacheKeys) > maxPackResources {
			continue
		}
		if manifest.State == "running" || manifest.State == "queued" {
			manifest.State = "incomplete"
			manifest.ErrorCode = "interrupted"
			changed[manifest.ID] = true
		} else if manifest.State == "complete" && manifest.Resources == nil {
			// Older manifests cannot prove which route resources completed. Force
			// one safe refresh rather than exposing contradictory per-resource state.
			manifest.State = "incomplete"
			manifest.ErrorCode = "legacy_manifest"
			changed[manifest.ID] = true
		}
		copyManifest := manifest
		m.packs[manifest.ID] = &copyManifest
	}
	return changed, nil
}

func (m *packManager) pruneExcessStoredPacks(changed map[string]bool) error {
	for len(m.packs) > maxStoredPackManifests {
		pruneID := m.oldestAutomaticPackLocked()
		if pruneID == "" {
			break
		}
		pruned := m.packs[pruneID]
		if err := m.removeManifestFile(pruned.ID); err != nil {
			return fmt.Errorf("repair interrupted automatic pack replacement: %w", err)
		}
		pruned.deleted = true
		delete(m.packs, pruneID)
		delete(changed, pruneID)
	}
	return nil
}

func (m *packManager) desiredStoredPins() (map[string][]string, map[string]struct{}) {
	desired := make(map[string][]string)
	unavailable := make(map[string]struct{})
	for _, manifest := range m.packs {
		seen := make(map[string]struct{}, len(manifest.CacheKeys))
		for _, key := range manifest.CacheKeys {
			if !validCacheKey(key) {
				unavailable[key] = struct{}{}
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			desired[key] = append(desired[key], manifest.ID)
		}
	}
	return desired, unavailable
}

func (m *packManager) persistRecoveredPacks(changed map[string]bool, unavailable map[string]struct{}) error {
	for _, manifest := range m.packs {
		for _, key := range manifest.CacheKeys {
			if _, missing := unavailable[key]; missing {
				if manifest.State != "incomplete" || manifest.ErrorCode != "missing_cache_entries" {
					manifest.State = "incomplete"
					manifest.ErrorCode = "missing_cache_entries"
					changed[manifest.ID] = true
				}
				break
			}
		}
		if changed[manifest.ID] {
			if err := m.persistLocked(manifest); err != nil {
				return fmt.Errorf("persist recovered pack %s: %w", manifest.ID, err)
			}
		}
	}
	return nil
}

func validPackID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func newPackID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (m *packManager) persistLocked(manifest *packManifest) error {
	if !m.server.cache.writable {
		return nil
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(raw) > maxPackManifestBytes {
		return errors.New("pack manifest exceeds storage limit")
	}
	return atomicWriteFileAt(m.server.cache.root, m.server.cache.tmpRel, filepath.Join(m.server.cache.packsRel, manifest.ID+".json"), raw)
}

func (m *packManager) removeManifestFile(id string) error {
	if !m.server.cache.writable {
		return nil
	}
	if err := m.server.cache.root.Remove(filepath.Join(m.server.cache.packsRel, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRootDir(m.server.cache.root, m.server.cache.packsRel)
}

func validatePackInput(input packInput) (bbox, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		return bbox{}, errors.New("name is required and must not exceed 100 characters")
	}
	if input.PaddingKM < 0 || input.PaddingKM > 100 || math.IsNaN(input.PaddingKM) {
		return bbox{}, errors.New("paddingKm must be between 0 and 100")
	}
	if input.ZoomMin < 0 || input.ZoomMax < input.ZoomMin || input.ZoomMax > 19 {
		return bbox{}, errors.New("zoom range must be ordered within 0..19")
	}
	if duplicateValue(input.Layers) || duplicateValue(input.Scopes) {
		return bbox{}, errors.New("layers and scopes must not contain duplicates")
	}
	for _, scope := range input.Scopes {
		switch scope {
		case "elevation", "routing", "surface", "pois", "fuel", "places":
		default:
			return bbox{}, fmt.Errorf("unknown data scope %q", scope)
		}
	}
	if input.BBox != nil && len(input.Route) != 0 {
		return bbox{}, errors.New("provide bbox or route, not both")
	}
	var bounds bbox
	if input.BBox != nil {
		bounds = *input.BBox
		if err := bounds.validate(20000, true); err != nil {
			return bbox{}, err
		}
	} else {
		if err := validatePoints(input.Route, 2, 5000); err != nil {
			return bbox{}, errors.New("route must contain 2..5000 valid coordinates")
		}
		bounds = routeBounds(input.Route)
	}
	return paddedBounds(bounds, input.PaddingKM), nil
}

func duplicateValue(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func routeBounds(route []coordinate) bbox {
	south, north := route[0].Lat, route[0].Lat
	longitudes := make([]float64, len(route))
	for i, point := range route {
		south = min(south, point.Lat)
		north = max(north, point.Lat)
		longitude := point.Lon
		if longitude < 0 {
			longitude += 360
		}
		longitudes[i] = longitude
	}
	sort.Float64s(longitudes)
	largestGap, gapIndex := -1.0, 0
	for i := range longitudes {
		next := longitudes[(i+1)%len(longitudes)]
		if i == len(longitudes)-1 {
			next += 360
		}
		if gap := next - longitudes[i]; gap > largestGap {
			largestGap, gapIndex = gap, i
		}
	}
	west := longitudes[(gapIndex+1)%len(longitudes)]
	east := longitudes[gapIndex]
	if west > 180 {
		west -= 360
	}
	if east > 180 {
		east -= 360
	}
	return bbox{South: south, West: west, North: north, East: east}
}

func paddedBounds(bounds bbox, paddingKM float64) bbox {
	latPad := paddingKM / 111.195
	mid := (bounds.South + bounds.North) / 2 * math.Pi / 180
	lonPad := paddingKM / (111.195 * math.Max(.05, math.Cos(mid)))
	bounds.South = max(-85.05112878, bounds.South-latPad)
	bounds.North = min(85.05112878, bounds.North+latPad)
	bounds.West = wrapLongitude(bounds.West - lonPad)
	bounds.East = wrapLongitude(bounds.East + lonPad)
	return bounds
}

func wrapLongitude(value float64) float64 {
	for value < -180 {
		value += 360
	}
	for value > 180 {
		value -= 360
	}
	return value
}

func enumerateTiles(bounds bbox, zoomMin, zoomMax, capCount int) ([]tileKey, error) {
	seen := make(map[tileKey]struct{})
	for z := zoomMin; z <= zoomMax; z++ {
		n := 1 << z
		yNorth := mercatorTileY(bounds.North, z)
		ySouth := mercatorTileY(bounds.South, z)
		intervals := [][2]float64{{bounds.West, bounds.East}}
		if bounds.West > bounds.East {
			intervals = [][2]float64{{bounds.West, 180}, {-180, bounds.East}}
		}
		for _, interval := range intervals {
			x0 := longitudeTileX(interval[0], z)
			x1 := longitudeTileX(interval[1], z)
			for x := x0; x <= x1; x++ {
				for y := yNorth; y <= ySouth; y++ {
					key := tileKey{z: z, x: min(x, n-1), y: y}
					seen[key] = struct{}{}
					if len(seen) > capCount {
						return nil, fmt.Errorf("pack exceeds %d resources", capCount)
					}
				}
			}
		}
	}
	tiles := make([]tileKey, 0, len(seen))
	for key := range seen {
		tiles = append(tiles, key)
	}
	sort.Slice(tiles, func(i, j int) bool {
		if tiles[i].z != tiles[j].z {
			return tiles[i].z < tiles[j].z
		}
		if tiles[i].x != tiles[j].x {
			return tiles[i].x < tiles[j].x
		}
		return tiles[i].y < tiles[j].y
	})
	return tiles, nil
}

func enumeratePackTiles(input packInput, bounds bbox, zoomMin, zoomMax, capCount int) ([]tileKey, error) {
	if len(input.Route) == 0 {
		return enumerateTiles(bounds, zoomMin, zoomMax, capCount)
	}
	seen := make(map[tileKey]struct{})
	for z := zoomMin; z <= zoomMax; z++ {
		for i := 1; i < len(input.Route); i++ {
			a, b := input.Route[i-1], input.Route[i]
			midLat := (a.Lat + b.Lat) / 2 * math.Pi / 180
			tileWidth := 40075016.686 * math.Max(.05, math.Cos(midLat)) / math.Exp2(float64(z))
			steps := max(1, int(math.Ceil(haversineMeters(a, b)/max(50, tileWidth/2))))
			previous := a
			for step := 1; step <= steps; step++ {
				current := interpolateCoordinate(a, b, float64(step)/float64(steps))
				segmentBounds := paddedBounds(routeBounds([]coordinate{previous, current}), input.PaddingKM)
				tiles, err := enumerateTiles(segmentBounds, z, z, maxPackResources)
				if err != nil {
					return nil, err
				}
				for _, key := range tiles {
					seen[key] = struct{}{}
					if len(seen) > capCount {
						return nil, fmt.Errorf("pack exceeds %d resources", capCount)
					}
				}
				previous = current
			}
		}
	}
	tiles := make([]tileKey, 0, len(seen))
	for key := range seen {
		tiles = append(tiles, key)
	}
	sort.Slice(tiles, func(i, j int) bool {
		if tiles[i].z != tiles[j].z {
			return tiles[i].z < tiles[j].z
		}
		if tiles[i].x != tiles[j].x {
			return tiles[i].x < tiles[j].x
		}
		return tiles[i].y < tiles[j].y
	})
	return tiles, nil
}

func interpolateCoordinate(a, b coordinate, ratio float64) coordinate {
	deltaLon := b.Lon - a.Lon
	if deltaLon > 180 {
		deltaLon -= 360
	} else if deltaLon < -180 {
		deltaLon += 360
	}
	return coordinate{Lat: a.Lat + (b.Lat-a.Lat)*ratio, Lon: wrapLongitude(a.Lon + deltaLon*ratio)}
}

func longitudeTileX(lon float64, zoom int) int {
	n := math.Exp2(float64(zoom))
	x := int(math.Floor((lon + 180) / 360 * n))
	return max(0, min(x, int(n)-1))
}

func mercatorTileY(lat float64, zoom int) int {
	lat = max(-85.05112878, min(85.05112878, lat))
	rad := lat * math.Pi / 180
	n := math.Exp2(float64(zoom))
	y := int(math.Floor((1 - math.Log(math.Tan(rad)+1/math.Cos(rad))/math.Pi) / 2 * n))
	return max(0, min(y, int(n)-1))
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func poiPackCategory(kind string) string {
	switch kind {
	case "fuel":
		return packResourceFuelStations
	case "water":
		return packResourceWater
	default:
		return packResourceCampsites
	}
}

func packProgressFromEstimate(estimate packEstimate) map[string]packResourceProgress {
	resources := make(map[string]packResourceProgress)
	add := func(category string, total int) {
		if total <= 0 {
			return
		}
		progress := resources[category]
		progress.Total += total
		resources[category] = progress
	}
	add(packResourceVectorMap, estimate.Counts["openfreemap-core"]+estimate.Counts["openfreemap-glyphs"]+estimate.Counts["openfreemap"]+estimate.Counts["openfreemap-raster"])
	add(packResourceElevation, estimate.Counts["elevation"])
	add(packResourceFuelPrices, estimate.Counts["fuel"])
	add(packResourceFuelStations, estimate.Counts["pois-fuel"])
	add(packResourceWater, estimate.Counts["pois-water"])
	add(packResourceCampsites, estimate.Counts["pois-camp"])
	add(packResourceRoute, estimate.Counts["routing"])
	add(packResourceSurface, estimate.Counts["surface"])
	add(packResourcePlaces, estimate.Counts["places"])
	add(packResourcePOIs, estimate.Counts["pois"])
	return resources
}

func updatePackResource(pack *packManifest, category string, failed bool, bytes int64, items int) {
	if pack.Resources == nil {
		pack.Resources = make(map[string]packResourceProgress)
	}
	progress := pack.Resources[category]
	progress.Done++
	progress.Bytes += bytes
	progress.Items += items
	if failed {
		progress.Failed++
	}
	pack.Resources[category] = progress
}

func packPOIBounds(input packInput, fallback bbox) ([]bbox, error) {
	if len(input.Route) == 0 {
		if err := fallback.validate(250, false); err != nil {
			return nil, err
		}
		return []bbox{fallback}, nil
	}

	// Overpass rejects broad searches. Group the route into bounded corridor
	// sections instead of turning a long diagonal trip into one enormous box.
	paddingKM := max(5.0, input.PaddingKM)
	chunkMeters := max(20.0, 220-2*paddingKM) * 1000
	sampleMeters := min(25_000.0, chunkMeters/4)
	var bounds []bbox
	appendBounds := func(points []coordinate) error {
		if len(points) == 0 {
			return nil
		}
		section := paddedBounds(routeBounds(points), paddingKM)
		sections := []bbox{section}
		if section.West > section.East {
			sections = []bbox{
				{South: section.South, West: section.West, North: section.North, East: 180},
				{South: section.South, West: -180, North: section.North, East: section.East},
			}
		}
		for _, section := range sections {
			if err := section.validate(250, false); err != nil {
				return err
			}
			bounds = append(bounds, section)
		}
		return nil
	}

	current := []coordinate{input.Route[0]}
	var sectionMeters float64
	appendPoint := func(point coordinate) error {
		previous := current[len(current)-1]
		if math.Abs(point.Lon-previous.Lon) > 180 {
			delta := point.Lon - previous.Lon
			boundary, opposite := -180.0, 180.0
			if delta < -180 {
				delta += 360
				boundary, opposite = 180, -180
			} else {
				delta -= 360
			}
			ratio := (boundary - previous.Lon) / delta
			latitude := previous.Lat + (point.Lat-previous.Lat)*ratio
			current = append(current, coordinate{Lat: latitude, Lon: boundary})
			if err := appendBounds(current); err != nil {
				return err
			}
			current = []coordinate{{Lat: latitude, Lon: opposite}, point}
			sectionMeters = haversineMeters(current[0], point)
			return nil
		}
		sectionMeters += haversineMeters(previous, point)
		current = append(current, point)
		if sectionMeters >= chunkMeters {
			if err := appendBounds(current); err != nil {
				return err
			}
			current = []coordinate{point}
			sectionMeters = 0
		}
		return nil
	}

	for index := 1; index < len(input.Route); index++ {
		a, b := input.Route[index-1], input.Route[index]
		steps := max(1, int(math.Ceil(haversineMeters(a, b)/sampleMeters)))
		for step := 1; step <= steps; step++ {
			if err := appendPoint(interpolateCoordinate(a, b, float64(step)/float64(steps))); err != nil {
				return nil, err
			}
		}
	}
	if len(current) > 1 || len(bounds) == 0 {
		if err := appendBounds(current); err != nil {
			return nil, err
		}
	}
	return bounds, nil
}

func packResponseItems(category string, body []byte) int {
	switch category {
	case packResourceFuelStations, packResourceWater, packResourceCampsites:
		var payload struct {
			Elements []json.RawMessage `json:"elements"`
		}
		if json.Unmarshal(body, &payload) == nil {
			return len(payload.Elements)
		}
	case packResourceFuelPrices:
		var payload struct {
			Stations []json.RawMessage `json:"ListaEESSPrecio"`
		}
		if json.Unmarshal(body, &payload) == nil {
			return len(payload.Stations)
		}
	}
	return 0
}

func (m *packManager) estimate(input packInput) (packEstimate, error) {
	return m.estimateContext(context.Background(), input)
}

func (m *packManager) estimateContext(ctx context.Context, input packInput) (packEstimate, error) {
	bounds, err := validatePackInput(input)
	if err != nil {
		return packEstimate{}, err
	}
	estimate := packEstimate{Counts: make(map[string]int), Scopes: make(map[string]cacheScopeStat), Bounds: bounds, MapTiles: make(map[string][]tileKey), RasterMapTiles: make(map[string][]tileKey)}
	for _, layer := range input.Layers {
		switch layer {
		case "osm", "opentopo", "cyclosm", "satellite", "relief", "hillshade":
			estimate.Blocked = append(estimate.Blocked, blockedPackResource{Layer: layer, Reason: "public provider permits passive caching only"})
		case "openfreemap":
			if m.server.openFreeMap != nil && !m.server.openFreeMap.waitForInitialActivation(ctx) {
				return packEstimate{}, ctx.Err()
			}
			if m.server.openFreeMap == nil || !m.server.openFreeMap.isActive() {
				estimate.Blocked = append(estimate.Blocked, blockedPackResource{Layer: layer, Reason: "map source is not available through the offline cache"})
				continue
			}
			if !m.server.openFreeMap.allowBulk {
				estimate.Blocked = append(estimate.Blocked, blockedPackResource{Layer: layer, Reason: "bounded trip-pack fetching is disabled"})
				continue
			}
			m.server.openFreeMap.mu.RLock()
			if !m.server.openFreeMap.durableCore {
				m.server.openFreeMap.mu.RUnlock()
				estimate.Blocked = append(estimate.Blocked, blockedPackResource{Layer: layer, Reason: "configured map style core is not persistently cacheable"})
				continue
			}
			coreKeys := append([]string(nil), m.server.openFreeMap.coreKeys...)
			coreCount := len(coreKeys)
			generation := m.server.openFreeMap.generation
			fontStacks := append([]string(nil), m.server.openFreeMap.fontStacks...)
			hasGlyphs := m.server.openFreeMap.glyphs != ""
			m.server.openFreeMap.mu.RUnlock()
			glyphMissing := 0
			if hasGlyphs {
				for _, font := range fontStacks {
					for _, rangeValue := range []string{"0-255", "256-511"} {
						resource := glyphPackResource{Font: font, Range: rangeValue}
						estimate.Glyphs = append(estimate.Glyphs, resource)
						estimate.Resources++
						estimate.Counts["openfreemap-glyphs"]++
						params := generation + ":glyph:" + font + ":" + rangeValue
						cacheKey := canonicalCacheKey(m.server.openFreeMap.policy.name, m.server.openFreeMap.policy.sourceFingerprint, http.MethodGet, params, "", nil)
						if entry, ok := m.server.cache.get(m.server.openFreeMap.policy.scope, cacheKey); ok && staleAllowed(entry.Meta, time.Now().UTC()) {
							estimate.Reused++
							estimate.ReusedBytes += entry.Meta.Length
						} else {
							glyphMissing++
						}
					}
				}
			}
			if hasGlyphs && len(fontStacks) > 0 {
				estimate.Dynamic = append(estimate.Dynamic, "map pack includes Latin glyph ranges; labels using other scripts remain on-demand")
			}
			estimate.Counts["openfreemap-core"] = coreCount
			estimate.Resources += coreCount
			for _, key := range coreKeys {
				entry, ok := m.server.cache.get(m.server.openFreeMap.policy.scope, key)
				if !ok || !staleAllowed(entry.Meta, time.Now().UTC()) {
					return packEstimate{}, errors.New("configured map style core is no longer complete in the cache")
				}
				estimate.Reused++
				estimate.ReusedBytes += entry.Meta.Length
			}
			if estimate.Resources >= maxPackResources {
				return packEstimate{}, fmt.Errorf("pack exceeds %d resources", maxPackResources)
			}
			m.server.openFreeMap.mu.RLock()
			vectorSources := make([]string, 0, len(m.server.openFreeMap.tiles))
			for source := range m.server.openFreeMap.tiles {
				vectorSources = append(vectorSources, source)
			}
			rasterSources := make(map[string]int, len(m.server.openFreeMap.rasters))
			for source := range m.server.openFreeMap.rasters {
				maxZoom, ok := m.server.openFreeMap.rasterMaxZoom[source]
				if !ok {
					maxZoom = 19
				}
				rasterSources[source] = maxZoom
			}
			m.server.openFreeMap.mu.RUnlock()
			var tiles []tileKey
			if len(vectorSources) > 0 {
				var err error
				tiles, err = enumeratePackTiles(input, bounds, input.ZoomMin, input.ZoomMax, maxPackResources-estimate.Resources)
				if err != nil {
					return packEstimate{}, err
				}
			}
			vectorMissing := 0
			for _, source := range vectorSources {
				estimate.MapTiles[source] = tiles
				estimate.Counts["openfreemap"] += len(tiles)
				estimate.Resources += len(tiles)
				for _, key := range tiles {
					params := fmt.Sprintf("%s:resource:%s:%d/%d/%d", generation, source, key.z, key.x, key.y)
					cacheKey := canonicalCacheKey(m.server.openFreeMap.policy.name, m.server.openFreeMap.policy.sourceFingerprint, http.MethodGet, params, "", nil)
					if entry, ok := m.server.cache.get(m.server.openFreeMap.policy.scope, cacheKey); ok && staleAllowed(entry.Meta, time.Now().UTC()) {
						estimate.Reused++
						estimate.ReusedBytes += entry.Meta.Length
					} else {
						vectorMissing++
					}
				}
			}
			rasterMissing := 0
			for source, sourceMaxZoom := range rasterSources {
				if sourceMaxZoom < input.ZoomMin {
					continue
				}
				rasterTiles, err := enumeratePackTiles(input, bounds, input.ZoomMin, min(input.ZoomMax, sourceMaxZoom), maxPackResources-estimate.Resources)
				if err != nil {
					return packEstimate{}, err
				}
				estimate.RasterMapTiles[source] = rasterTiles
				estimate.Counts["openfreemap-raster"] += len(rasterTiles)
				estimate.Resources += len(rasterTiles)
				for _, key := range rasterTiles {
					params := fmt.Sprintf("%s:resource:%s:%d/%d/%d", generation, source, key.z, key.x, key.y)
					cacheKey := canonicalCacheKey(m.server.openFreeMap.policy.name, m.server.openFreeMap.policy.sourceFingerprint, http.MethodGet, params, "", nil)
					if entry, ok := m.server.cache.get(m.server.openFreeMap.policy.scope, cacheKey); ok && staleAllowed(entry.Meta, time.Now().UTC()) {
						estimate.Reused++
						estimate.ReusedBytes += entry.Meta.Length
					} else {
						rasterMissing++
					}
				}
			}
			mapBytes := (int64(vectorMissing) * 50 << 10) + (int64(rasterMissing) * 80 << 10) + (int64(glyphMissing) * 40 << 10)
			estimate.EstimatedBytes += mapBytes
			estimate.GenericBytes += mapBytes
			estimate.Scopes["maps-openfreemap"] = cacheScopeStat{Bytes: mapBytes, Entries: estimate.Counts["openfreemap"] + estimate.Counts["openfreemap-raster"] + estimate.Counts["openfreemap-glyphs"] + coreCount}
		default:
			return packEstimate{}, fmt.Errorf("unknown layer %q", layer)
		}
	}
	if containsString(input.Scopes, "elevation") {
		if m.server.elevation.tiles == nil || m.server.elevation.tiles.cacheDir == "" {
			return packEstimate{}, errors.New("elevation packs require Terrarium tile mode with a persistent tile cache")
		}
		tiles, err := enumeratePackTiles(input, bounds, m.server.elevation.tiles.zoom, m.server.elevation.tiles.zoom, maxPackResources-estimate.Resources)
		if err != nil {
			return packEstimate{}, err
		}
		if len(tiles) > maxElevationPackEntries {
			return packEstimate{}, fmt.Errorf("elevation pack exceeds %d tiles", maxElevationPackEntries)
		}
		estimate.ElevationTiles = tiles
		estimate.Counts["elevation"] = len(tiles)
		estimate.Resources += len(tiles)
		elevationReused := 0
		var elevationReusedBytes int64
		for _, key := range tiles {
			if m.server.elevation.tiles.have(key) {
				estimate.Reused++
				elevationReused++
				if size, ok := m.server.elevation.tiles.diskFileSize(key); ok {
					estimate.ReusedBytes += size
					elevationReusedBytes += size
				}
			}
		}
		missing := len(tiles) - elevationReused
		estimatedTileBytes := int64(missing)*(120<<10) + elevationReusedBytes
		if estimatedTileBytes > maxElevationPackBytes {
			return packEstimate{}, errors.New("elevation pack exceeds its byte limit")
		}
		if estimatedTileBytes > m.server.elevation.tiles.diskQuota() {
			return packEstimate{}, errors.New("elevation pack exceeds the configured tile cache quota")
		}
		elevationBytes := int64(missing) * 120 << 10
		estimate.Scopes["elevation"] = cacheScopeStat{Bytes: elevationBytes, Entries: len(tiles)}
		estimate.EstimatedBytes += elevationBytes
	}
	if containsString(input.Scopes, "fuel") {
		p := m.server.providers["fuel"]
		key := canonicalCacheKey(p.name, p.sourceFingerprint, http.MethodGet, "national-snapshot", "", nil)
		estimate.Counts["fuel"] = 1
		estimate.Resources++
		fuelBytes := int64(8 << 20)
		if entry, ok := m.server.cache.get(p.scope, key); ok && staleAllowed(entry.Meta, time.Now().UTC()) {
			estimate.Reused++
			estimate.ReusedBytes += entry.Meta.Length
			fuelBytes = 0
		}
		estimate.Scopes["fuel"] = cacheScopeStat{Bytes: fuelBytes, Entries: 1}
		estimate.EstimatedBytes += fuelBytes
		estimate.GenericBytes += fuelBytes
	}
	if containsString(input.Scopes, "pois") {
		policy := m.server.providers["pois"]
		poiBytes := int64(0)
		searchBounds, boundsErr := packPOIBounds(input, bounds)
		if boundsErr != nil {
			reason := "route area cannot be split into bounded POI searches"
			for _, blockedKind := range []string{"fuel", "water", "camp"} {
				estimate.Blocked = append(estimate.Blocked, blockedPackResource{Resource: poiPackCategory(blockedKind), Reason: reason})
			}
		} else {
			for _, kind := range []string{"fuel", "water", "camp"} {
				for _, searchBound := range searchBounds {
					request, err := buildPOIRequest(policy, kind, searchBound)
					if err != nil {
						return packEstimate{}, err
					}
					estimate.POIRequests = append(estimate.POIRequests, poiPackResource{Category: poiPackCategory(kind), Request: request})
					estimate.Counts["pois-"+kind]++
					estimate.Resources++
					key := canonicalCacheKey(policy.name, policy.sourceFingerprint, request.method, request.params, request.language, request.body)
					if entry, ok := m.server.cache.get(policy.scope, key); ok && staleAllowed(entry.Meta, time.Now().UTC()) {
						estimate.Reused++
						estimate.ReusedBytes += entry.Meta.Length
					} else {
						estimate.EstimatedBytes += 1 << 20
						estimate.GenericBytes += 1 << 20
						poiBytes += 1 << 20
					}
				}
			}
		}
		if len(estimate.POIRequests) != 0 {
			estimate.Scopes["pois"] = cacheScopeStat{Bytes: poiBytes, Entries: len(estimate.POIRequests)}
		}
	}
	for _, scope := range []string{"routing", "surface", "pois", "places"} {
		if !containsString(input.Scopes, scope) {
			continue
		}
		if scope == "pois" && len(estimate.POIRequests) != 0 {
			continue
		}
		keys, bytes := m.server.cache.retainedKeys(scope, time.Now().UTC())
		category := map[string]string{
			"routing": packResourceRoute,
			"surface": packResourceSurface,
			"pois":    packResourcePOIs,
			"places":  packResourcePlaces,
		}[scope]
		for _, key := range keys {
			estimate.ExistingKeys = append(estimate.ExistingKeys, existingPackResource{Category: category, Key: key})
		}
		estimate.Counts[scope] = len(keys)
		estimate.Scopes[scope] = cacheScopeStat{Bytes: bytes, Entries: len(keys)}
		estimate.Resources += len(keys)
		estimate.Reused += len(keys)
		estimate.ReusedBytes += bytes
		detail := map[string]string{
			"routing": "Routes protect exact results already in the cache; the pack does not calculate new routes",
			"surface": "Surface protects exact analyses already in the cache; the pack does not analyze new roads",
			"pois":    "POIs protect exact searches already in the cache; the pack does not run new POI queries for this area",
			"places":  "Places protect exact searches already in the cache; the pack does not search for new places",
		}[scope]
		estimate.Dynamic = append(estimate.Dynamic, detail)
	}
	if estimate.Resources > maxPackResources {
		return packEstimate{}, fmt.Errorf("pack exceeds %d resources", maxPackResources)
	}
	stats := m.server.cache.stats()
	estimate.RemainingQuota = max(0, stats.Quota-stats.Bytes-stats.Reserved)
	estimate.FinalBytes = stats.Bytes + estimate.GenericBytes
	estimate.Detail = strings.Join(estimate.Dynamic, "; ")
	return estimate, nil
}

func (m *packManager) start(input packInput) (*packManifest, packEstimate, error) {
	return m.startContext(context.Background(), input)
}

func (m *packManager) startContext(ctx context.Context, input packInput) (*packManifest, packEstimate, error) {
	if !m.server.cache.writable {
		return nil, packEstimate{}, errors.New("persistent offline cache is disabled")
	}
	input.Name = strings.TrimSpace(input.Name)
	m.mu.Lock()
	if existing := m.matchingPackLocked(input); existing != nil {
		copyManifest := m.publicCopyLocked(existing)
		m.mu.Unlock()
		estimate, err := m.estimateContext(ctx, input)
		return &copyManifest, estimate, err
	}
	active, stored := m.activeLocked(), len(m.packs)
	pruneID := ""
	if input.Automatic && stored >= maxStoredPackManifests {
		pruneID = m.oldestAutomaticPackLocked()
	}
	m.mu.Unlock()
	if active >= maxActivePackJobs {
		return nil, packEstimate{}, fmt.Errorf("at most %d pack jobs may be active", maxActivePackJobs)
	}
	if stored >= maxStoredPackManifests && pruneID == "" {
		return nil, packEstimate{}, fmt.Errorf("at most %d pack manifests may be retained", maxStoredPackManifests)
	}
	estimate, err := m.estimateContext(ctx, input)
	if err != nil {
		return nil, packEstimate{}, err
	}
	if estimate.Resources == 0 {
		return nil, estimate, errors.New("pack has no provider-permitted resources to prepare")
	}
	if estimate.GenericBytes > estimate.RemainingQuota {
		return nil, estimate, errors.New("pack would exceed the generic cache quota")
	}
	if err := requirePackDiskSpace(m.server.cache.dir, estimate.GenericBytes); err != nil {
		return nil, estimate, err
	}
	if elevation := estimate.Scopes["elevation"].Bytes; elevation > 0 {
		if err := requirePackDiskSpace(m.server.elevation.tiles.cacheDir, elevation); err != nil {
			return nil, estimate, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, estimate, err
	}
	id, err := newPackID()
	if err != nil {
		return nil, estimate, err
	}
	now := time.Now().UTC()
	manifest := &packManifest{ID: id, Name: input.Name, State: "queued", CreatedAt: now, UpdatedAt: now, Total: estimate.Resources, Resources: packProgressFromEstimate(estimate), Input: input}
	jobCtx, cancel := context.WithTimeout(m.server.ctx, m.jobTimeout)
	manifest.cancel = cancel
	var pruned *packManifest
	m.mu.Lock()
	if existing := m.matchingPackLocked(input); existing != nil {
		copyManifest := m.publicCopyLocked(existing)
		m.mu.Unlock()
		cancel()
		return &copyManifest, estimate, nil
	}
	if m.activeLocked() >= maxActivePackJobs {
		m.mu.Unlock()
		cancel()
		return nil, estimate, errors.New("pack capacity was reached while estimating")
	}
	if len(m.packs) >= maxStoredPackManifests {
		if input.Automatic {
			pruneID = m.oldestAutomaticPackLocked()
		}
		if pruneID == "" {
			m.mu.Unlock()
			cancel()
			return nil, estimate, errors.New("pack capacity was reached while estimating")
		}
		pruned = m.packs[pruneID]
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		cancel()
		return nil, estimate, err
	}
	if err := m.persistLocked(manifest); err != nil {
		m.mu.Unlock()
		cancel()
		return nil, estimate, err
	}
	if pruned != nil {
		cleanup := m.cleanupPack
		if m.replacementCleanup != nil {
			cleanup = m.replacementCleanup
		}
		if err := cleanup(pruned); err != nil {
			rollbackErr := m.restoreReplacedPackLocked(pruned, manifest)
			m.mu.Unlock()
			cancel()
			return nil, estimate, errors.Join(fmt.Errorf("replace oldest automatic pack: %w", err), rollbackErr)
		}
		pruned.deleted = true
		delete(m.packs, pruned.ID)
	}
	m.packs[id] = manifest
	copyManifest := m.publicCopyLocked(manifest)
	m.mu.Unlock()
	m.server.wg.Add(1)
	go m.run(jobCtx, cancel, manifest, estimate)
	return &copyManifest, estimate, nil
}

func (m *packManager) matchingPackLocked(input packInput) *packManifest {
	for _, pack := range m.packs {
		candidate := pack.Input
		requested := input
		if requested.Automatic && !candidate.Automatic {
			// Reuse identical pre-marker packs without changing their retention
			// semantics. Only explicitly automatic packs are evictable.
			requested.Automatic = false
		}
		if (pack.State == "queued" || pack.State == "running" || pack.State == "complete") && reflect.DeepEqual(candidate, requested) {
			return pack
		}
	}
	return nil
}

func (m *packManager) restoreReplacedPackLocked(pruned, replacement *packManifest) error {
	var restoreErr error
	restoreErr = errors.Join(restoreErr, m.persistLocked(pruned))
	for _, key := range pruned.CacheKeys {
		restoreErr = errors.Join(restoreErr, m.server.cache.pin(key, pruned.ID, true))
	}
	restoreErr = errors.Join(restoreErr, m.cleanupPack(replacement))
	return restoreErr
}

func (m *packManager) activeLocked() int {
	active := 0
	for _, manifest := range m.packs {
		if manifest.State == "queued" || manifest.State == "running" {
			active++
		}
	}
	return active
}

func (m *packManager) oldestAutomaticPackLocked() string {
	var oldest *packManifest
	for _, manifest := range m.packs {
		if !manifest.Input.Automatic || manifest.State == "queued" || manifest.State == "running" {
			continue
		}
		if oldest == nil || manifest.UpdatedAt.Before(oldest.UpdatedAt) {
			oldest = manifest
		}
	}
	if oldest == nil {
		return ""
	}
	return oldest.ID
}

func requirePackDiskSpace(path string, required int64) error {
	if path == "" || required <= 0 {
		return nil
	}
	available, supported, err := availableDiskBytes(path)
	if err != nil {
		return fmt.Errorf("check available disk space: %w", err)
	}
	const safetyMargin = uint64(64 << 20)
	if supported && available < uint64(required)+safetyMargin {
		return errors.New("pack would leave less than 64 MiB of free disk space")
	}
	return nil
}

func (m *packManager) run(ctx context.Context, cancel context.CancelFunc, manifest *packManifest, estimate packEstimate) {
	defer m.server.wg.Done()
	defer cancel()
	if !m.update(manifest, func(p *packManifest) { p.State = "running" }) {
		return
	}
	markIncomplete := func(code string) {
		m.update(manifest, func(p *packManifest) {
			p.State = "incomplete"
			p.ErrorCode = code
			p.cancel = nil
		})
	}
	fail := func() bool {
		switch ctx.Err() {
		case nil:
			return false
		case context.DeadlineExceeded:
			markIncomplete("deadline")
		default:
			markIncomplete("cancelled")
		}
		return true
	}
	recordFailure := func(category string, err error) bool {
		if !m.update(manifest, func(p *packManifest) {
			p.Done++
			p.Failures++
			updatePackResource(p, category, true, 0, 0)
		}) {
			return false
		}
		if fail() {
			return false
		}
		var budgetErr packBudgetError
		if errors.As(err, &budgetErr) || strings.Contains(err.Error(), "admission") || strings.Contains(err.Error(), "quota") || strings.Contains(err.Error(), "entry limit") {
			markIncomplete("resource_limit")
			return false
		}
		return true
	}
	budget := &packAdmissionBudget{limit: estimate.RemainingQuota}
	acceptResponse := func(category string, response cachedResponse, fetchErr error) bool {
		if fetchErr != nil {
			return recordFailure(category, fetchErr)
		}
		pinErr := m.server.cache.pin(response.Key, manifest.ID, true)
		if pinErr != nil {
			return recordFailure(category, pinErr)
		}
		applied := m.update(manifest, func(pack *packManifest) {
			pack.Done++
			pack.Bytes += response.AdmittedBytes
			updatePackResource(pack, category, false, response.AdmittedBytes, packResponseItems(category, response.Body))
			pack.CacheKeys = appendUnique(pack.CacheKeys, response.Key)
		})
		m.releaseRejectedPin(manifest, response.Key, nil, applied)
		return applied
	}
	pinExisting := func(category, key string) bool {
		pinErr := m.server.cache.pin(key, manifest.ID, true)
		if pinErr != nil {
			return recordFailure(category, pinErr)
		}
		applied := m.update(manifest, func(pack *packManifest) {
			pack.Done++
			updatePackResource(pack, category, false, 0, 0)
			pack.CacheKeys = appendUnique(pack.CacheKeys, key)
		})
		m.releaseRejectedPin(manifest, key, nil, applied)
		return applied
	}
	var elevationPackBytes int64
	elevationByteLimit := int64(maxElevationPackBytes)
	if m.server.elevation.tiles != nil {
		elevationByteLimit = min(elevationByteLimit, m.server.elevation.tiles.diskQuota())
	}
	for _, key := range estimate.ElevationTiles {
		if fail() {
			return
		}
		wasCached := m.server.elevation.tiles.have(key)
		_, err := m.server.elevation.tiles.grid(ctx, key)
		var tileBytes int64
		if err == nil {
			if size, ok := m.server.elevation.tiles.diskFileSize(key); ok {
				tileBytes = size
			}
			if elevationPackBytes+tileBytes > elevationByteLimit {
				err = errors.New("elevation pack exceeded its byte limit")
			}
		}
		newBytes := tileBytes
		if wasCached {
			newBytes = 0
		}
		if !m.update(manifest, func(p *packManifest) {
			p.Done++
			if err != nil {
				p.Failures++
			}
			p.Bytes += newBytes
			updatePackResource(p, packResourceElevation, err != nil, newBytes, 0)
		}) {
			return
		}
		if err != nil {
			if fail() {
				return
			}
			if elevationPackBytes+tileBytes > elevationByteLimit {
				markIncomplete("resource_limit")
				return
			}
			continue
		}
		elevationPackBytes += tileBytes
	}
	if estimate.Counts["fuel"] > 0 {
		if fail() {
			return
		}
		p := m.server.providers["fuel"]
		response, err := m.server.outbound.do(ctx, cachedRequest{policy: p, method: http.MethodGet, url: p.baseURL.String(), params: "national-snapshot", cacheable: true, headers: map[string]string{"Accept": "application/json"}, validate: validateFuelResponse, admit: budget.admit, cancelWithCaller: true})
		if !acceptResponse(packResourceFuelPrices, response, err) {
			return
		}
	}
	for _, resource := range estimate.POIRequests {
		if fail() {
			return
		}
		request := resource.Request
		request.admit = budget.admit
		request.cancelWithCaller = true
		response, err := m.server.outbound.do(ctx, request)
		if !acceptResponse(resource.Category, response, err) {
			return
		}
	}
	if m.server.openFreeMap != nil {
		m.server.openFreeMap.mu.RLock()
		coreKeys := append([]string(nil), m.server.openFreeMap.coreKeys...)
		generation := m.server.openFreeMap.generation
		m.server.openFreeMap.mu.RUnlock()
		if estimate.Counts["openfreemap-core"] > 0 {
			for _, key := range coreKeys {
				if fail() {
					return
				}
				if !pinExisting(packResourceVectorMap, key) {
					return
				}
			}
		}
		m.server.openFreeMap.mu.RLock()
		glyphTemplate := m.server.openFreeMap.glyphs
		m.server.openFreeMap.mu.RUnlock()
		for _, glyph := range estimate.Glyphs {
			if fail() {
				return
			}
			endpoint := strings.NewReplacer("{fontstack}", url.PathEscape(glyph.Font), "{range}", glyph.Range).Replace(glyphTemplate)
			response, err := m.server.openFreeMap.fetchWithAdmission(ctx, endpoint, generation+":glyph:"+glyph.Font+":"+glyph.Range, []string{"application/x-protobuf", "application/octet-stream", "application/vnd.mapbox-vector-tile"}, budget.admit)
			if !acceptResponse(packResourceVectorMap, response, err) {
				return
			}
		}
		for source, tiles := range estimate.MapTiles {
			m.server.openFreeMap.mu.RLock()
			template := m.server.openFreeMap.tiles[source]
			m.server.openFreeMap.mu.RUnlock()
			for _, key := range tiles {
				if fail() {
					return
				}
				endpoint := strings.NewReplacer("{z}", fmt.Sprint(key.z), "{x}", fmt.Sprint(key.x), "{y}", fmt.Sprint(key.y)).Replace(template)
				response, err := m.server.openFreeMap.fetchWithAdmission(ctx, endpoint, fmt.Sprintf("%s:resource:%s:%d/%d/%d", generation, source, key.z, key.x, key.y), []string{"application/vnd.mapbox-vector-tile", "application/x-protobuf", "application/octet-stream"}, budget.admit)
				if !acceptResponse(packResourceVectorMap, response, err) {
					return
				}
			}
		}
		for source, tiles := range estimate.RasterMapTiles {
			m.server.openFreeMap.mu.RLock()
			template := m.server.openFreeMap.rasters[source]
			m.server.openFreeMap.mu.RUnlock()
			for _, key := range tiles {
				if fail() {
					return
				}
				endpoint := strings.NewReplacer("{z}", fmt.Sprint(key.z), "{x}", fmt.Sprint(key.x), "{y}", fmt.Sprint(key.y)).Replace(template)
				response, err := m.server.openFreeMap.fetchWithAdmission(ctx, endpoint, fmt.Sprintf("%s:resource:%s:%d/%d/%d", generation, source, key.z, key.x, key.y), []string{"image/png", "image/jpeg", "image/webp"}, budget.admit)
				if !acceptResponse(packResourceVectorMap, response, err) {
					return
				}
			}
		}
	}
	for _, resource := range estimate.ExistingKeys {
		if fail() {
			return
		}
		if !pinExisting(resource.Category, resource.Key) {
			return
		}
	}
	m.update(manifest, func(p *packManifest) {
		if p.Failures == 0 && p.Done == p.Total {
			p.State = "complete"
		} else {
			p.State = "incomplete"
			p.ErrorCode = "resource_failures"
		}
		p.cancel = nil
	})
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func (m *packManager) releaseRejectedPin(manifest *packManifest, key string, pinErr error, applied bool) {
	if !applied && pinErr == nil {
		_ = m.server.cache.pin(key, manifest.ID, false)
	}
}

func (m *packManager) update(manifest *packManifest, change func(*packManifest)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if manifest.deleted {
		return false
	}
	change(manifest)
	manifest.UpdatedAt = time.Now().UTC()
	if err := m.persistLocked(manifest); err != nil {
		manifest.State = "incomplete"
		manifest.ErrorCode = "storage_error"
		if manifest.cancel != nil {
			manifest.cancel()
			manifest.cancel = nil
		}
		return false
	}
	return true
}

func (m *packManager) summaries() []packSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]packSummary, 0, len(m.packs))
	for _, p := range m.packs {
		out = append(out, m.summaryLocked(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAfter(out[j]) })
	return out
}

func (p packSummary) CreatedAfter(other packSummary) bool { return p.UpdatedAt.After(other.UpdatedAt) }

func (m *packManager) publicManifest(id string) (packSummary, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.packs[id]
	if p == nil {
		return packSummary{}, false
	}
	return m.summaryLocked(p), true
}

func clonePackResources(resources map[string]packResourceProgress) map[string]packResourceProgress {
	if len(resources) == 0 {
		return nil
	}
	cloned := make(map[string]packResourceProgress, len(resources))
	for category, progress := range resources {
		cloned[category] = progress
	}
	return cloned
}

func (m *packManager) summaryLocked(p *packManifest) packSummary {
	var bounds *bbox
	if p.Input.BBox != nil && p.Input.BBox.validate(20000, true) == nil {
		copy := *p.Input.BBox
		bounds = &copy
	}
	return packSummary{ID: p.ID, Name: p.Name, State: p.State, BBox: bounds, Done: p.Done, Total: p.Total, Failures: p.Failures, Bytes: p.Bytes, Resources: clonePackResources(p.Resources), ErrorCode: p.ErrorCode, Error: p.ErrorCode, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

func (m *packManager) publicCopyLocked(p *packManifest) packManifest {
	copyManifest := *p
	copyManifest.Input = packInput{}
	copyManifest.CacheKeys = nil
	copyManifest.Resources = clonePackResources(p.Resources)
	return copyManifest
}

func (m *packManager) cancel(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.packs[id]
	if p == nil {
		return false
	}
	if p.cancel != nil {
		p.cancel()
	}
	return true
}

func (m *packManager) delete(id string) (bool, error) {
	m.mu.Lock()
	p := m.packs[id]
	if p == nil {
		m.mu.Unlock()
		return false, nil
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.deleted = true
	delete(m.packs, id)
	m.mu.Unlock()
	return true, m.cleanupPack(p)
}

func (m *packManager) cleanupPack(p *packManifest) error {
	var persistenceErr error
	for _, key := range p.CacheKeys {
		persistenceErr = errors.Join(persistenceErr, m.server.cache.pin(key, p.ID, false))
	}
	persistenceErr = errors.Join(persistenceErr, m.removeManifestFile(p.ID))
	return persistenceErr
}

func (s *Server) handleListPacks(w http.ResponseWriter, _ *http.Request) {
	noStoreJSON(w, http.StatusOK, s.packs.summaries())
}

func (s *Server) handleEstimatePack(w http.ResponseWriter, r *http.Request) {
	var input packInput
	if err := decodeJSONBody(w, r, 1<<20, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	estimate, err := s.packs.estimateContext(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	noStoreJSON(w, http.StatusOK, estimate)
}

func (s *Server) handleCreatePack(w http.ResponseWriter, r *http.Request) {
	var input packInput
	if err := decodeJSONBody(w, r, 1<<20, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	manifest, _, err := s.packs.startContext(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	summary, _ := s.packs.publicManifest(manifest.ID)
	noStoreJSON(w, http.StatusAccepted, summary)
}

func (s *Server) handleGetPack(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !validPackID(id) {
		writeError(w, http.StatusBadRequest, "Invalid pack ID")
		return
	}
	manifest, ok := s.packs.publicManifest(id)
	if !ok {
		writeError(w, http.StatusNotFound, "Pack not found")
		return
	}
	noStoreJSON(w, http.StatusOK, manifest)
}

func (s *Server) handleCancelPack(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !validPackID(id) || !s.packs.cancel(id) {
		writeError(w, http.StatusNotFound, "Pack not found")
		return
	}
	noStoreJSON(w, http.StatusAccepted, map[string]string{"state": "cancelling"})
}

func (s *Server) handleDeletePack(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !validPackID(id) {
		writeError(w, http.StatusNotFound, "Pack not found")
		return
	}
	deleted, err := s.packs.delete(id)
	if !deleted {
		writeError(w, http.StatusNotFound, "Pack not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Pack was removed but storage cleanup failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
