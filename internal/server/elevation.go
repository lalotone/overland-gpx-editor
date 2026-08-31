package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

/*
 * Elevation lookups.
 *
 * Two API providers, one response shape. Open-Meteo is the no-setup fallback
 * when terrain tiles are disabled; a self-hosted opentopodata instance can
 * provide better data (30 m postings against Copernicus 90 m — see
 * docs/ACCURACY.md) and takes over as soon as its host is set.
 */

// Both upstreams cap a single request at 100 coordinates.
const maxLocationsPerRequest = 100

const (
	maxElevationLocations       = 6000
	maxElevationRequestDuration = 2 * time.Minute
)

// The frontend's ceiling is 6000 points (MAX_ELEVATION_LOOKUPS), each encoding
// to well under 200 bytes.
const maxElevationBodyBytes = 4 << 20

// Public Open-Meteo endpoint. A variable so tests can point it at a stub.
var openMeteoURL = "https://api.open-meteo.com/v1/elevation"

const userAgent = "gpx-editor (https://github.com/lalotone/overland-gpx-editor)"

// Datasets a self-hosted opentopodata instance may be asked for. Anything else
// falls back to the configured default rather than being forwarded — the
// dataset lands in the upstream URL path.
var allowedDatasets = map[string]bool{
	"srtm90m":  true,
	"srtm30m":  true,
	"eudem25m": true,
	"aster30m": true,
	"mapzen":   true,
}

// Open-Meteo serves Copernicus DEM GLO-90 and takes no dataset parameter.
const openMeteoDataset = "copernicus90m"

// point is a validated coordinate. Parsing up front keeps malformed input out
// of the upstream URL entirely.
type point struct {
	lat, lon float64
}

func (p point) String() string {
	return strconv.FormatFloat(p.lat, 'f', -1, 64) + "," + strconv.FormatFloat(p.lon, 'f', -1, 64)
}

type elevationProxy struct {
	// tiles reads elevation out of terrain-RGB rasters. It is the default
	// source: higher resolution than the public API, no per-point quota, and
	// it works offline once its cache is warm. Set only when no ElevationHost
	// is configured.
	tiles *tileStore
	// host is a self-hosted opentopodata-style service. Empty selects
	// Open-Meteo.
	host           string
	defaultDataset string
	client         *http.Client
	outbound       *outboundClient
	policy         *providerPolicy
}

type elevationCacheObservationKey struct{}

type elevationCacheObservation struct {
	response cachedResponse
}

func (o *elevationCacheObservation) record(response cachedResponse) {
	priority := map[string]int{"hit": 1, "miss": 2, "revalidated": 3, "stale": 4, "bypass": 5}
	if o.response.State == "" || priority[response.State] > priority[o.response.State] {
		o.response = response
	}
}

func (e *elevationProxy) usesOpenMeteo() bool { return e.tiles == nil && e.host == "" }

// dataset reports the effective dataset name for a request.
func (e *elevationProxy) dataset(requested string) string {
	if e.tiles != nil {
		return tileDataset
	}
	if e.usesOpenMeteo() {
		return openMeteoDataset
	}
	if allowedDatasets[requested] {
		return requested
	}
	return e.defaultDataset
}

// lookup returns one elevation per point, in order. A nil entry means the
// service had no value there — never a zero, which would read as sea level.
func (e *elevationProxy) lookup(ctx context.Context, points []point, dataset string) ([]*float64, error) {
	if e.tiles != nil {
		return e.tiles.lookup(ctx, points)
	}
	if e.usesOpenMeteo() {
		return e.lookupOpenMeteo(ctx, points)
	}
	return e.lookupOpenTopoData(ctx, points, dataset)
}

func (e *elevationProxy) get(ctx context.Context, endpoint string) ([]byte, error) {
	if e.outbound != nil && e.policy != nil {
		response, err := e.outbound.do(ctx, cachedRequest{
			policy: e.policy, method: http.MethodGet, url: endpoint,
			params: endpoint, cacheable: true,
			validate: e.validateResponse,
		})
		if err != nil {
			return nil, err
		}
		if observation, ok := ctx.Value(elevationCacheObservationKey{}).(*elevationCacheObservation); ok {
			observation.record(response)
		}
		return response.Body, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, fmt.Errorf("elevation response content encoding %q is not accepted", encoding)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxElevationBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxElevationBodyBytes {
		return nil, errors.New("elevation response exceeds provider limit")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("elevation service returned %d: %s",
			resp.StatusCode, strings.TrimSpace(truncate(string(body), 200)))
	}
	if err := e.validateResponse(body); err != nil {
		return nil, err
	}
	return body, nil
}

func (e *elevationProxy) validateResponse(body []byte) error {
	if e.usesOpenMeteo() {
		var payload struct {
			Elevation json.RawMessage `json:"elevation"`
			Error     bool            `json:"error"`
			Reason    string          `json:"reason"`
		}
		if json.Unmarshal(body, &payload) != nil {
			return errors.New("malformed Open-Meteo elevation response")
		}
		if payload.Error {
			if payload.Reason != "" {
				return errors.New(payload.Reason)
			}
			return errors.New("Open-Meteo rejected the elevation request")
		}
		var elevations []*float64
		if len(payload.Elevation) == 0 || json.Unmarshal(payload.Elevation, &elevations) != nil {
			return errors.New("Open-Meteo returned an invalid elevation list")
		}
		return nil
	}
	var payload struct {
		Results json.RawMessage `json:"results"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Results) == 0 {
		return errors.New("opentopodata returned an invalid elevation response")
	}
	var results []struct {
		Elevation *float64 `json:"elevation"`
	}
	if json.Unmarshal(payload.Results, &results) != nil {
		return errors.New("opentopodata returned an invalid elevation result list")
	}
	return nil
}

// lookupOpenTopoData talks to an opentopodata-style API:
//
//	GET <host>/v1/<dataset>?locations=lat,lon|lat,lon
//	→ {"results": [{"elevation": 512.0}, …]}
func (e *elevationProxy) lookupOpenTopoData(ctx context.Context, points []point, dataset string) ([]*float64, error) {
	locations := make([]string, len(points))
	for i, p := range points {
		locations[i] = p.String()
	}
	endpoint := fmt.Sprintf("%s/v1/%s?locations=%s",
		strings.TrimSuffix(e.host, "/"),
		url.PathEscape(dataset),
		url.QueryEscape(strings.Join(locations, "|")))

	body, err := e.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	var decoded struct {
		Results []struct {
			Elevation *float64 `json:"elevation"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("malformed elevation response: %w", err)
	}

	out := make([]*float64, len(points))
	for i := range out {
		if i < len(decoded.Results) {
			out[i] = decoded.Results[i].Elevation
		}
	}
	return out, nil
}

// lookupOpenMeteo talks to the public Open-Meteo elevation API:
//
//	GET .../v1/elevation?latitude=42.1,42.2&longitude=-0.4,-0.5
//	→ {"elevation": [512.0, 498.5]}
func (e *elevationProxy) lookupOpenMeteo(ctx context.Context, points []point) ([]*float64, error) {
	lats := make([]string, len(points))
	lons := make([]string, len(points))
	for i, p := range points {
		lats[i] = strconv.FormatFloat(p.lat, 'f', -1, 64)
		lons[i] = strconv.FormatFloat(p.lon, 'f', -1, 64)
	}
	endpoint := fmt.Sprintf("%s?latitude=%s&longitude=%s",
		openMeteoURL,
		url.QueryEscape(strings.Join(lats, ",")),
		url.QueryEscape(strings.Join(lons, ",")))

	body, err := e.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	var decoded struct {
		Elevation []*float64 `json:"elevation"`
		Reason    string     `json:"reason"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("malformed elevation response: %w", err)
	}
	if decoded.Reason != "" {
		return nil, errors.New("elevation service: " + decoded.Reason)
	}

	out := make([]*float64, len(points))
	for i := range out {
		if i < len(decoded.Elevation) {
			out[i] = decoded.Elevation[i]
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

/* -- Responses -------------------------------------------------------- */

type elevationResult struct {
	Elevation *float64 `json:"elevation"`
}

type elevationResponse struct {
	Results []elevationResult `json:"results"`
	Dataset string            `json:"dataset"`
}

func toResults(values []*float64) []elevationResult {
	out := make([]elevationResult, len(values))
	for i, v := range values {
		out[i] = elevationResult{Elevation: v}
	}
	return out
}

/* -- GET /elevation --------------------------------------------------- */

func (s *Server) handleElevation(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := parsePoint(q.Get("lat"), q.Get("lon"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	dataset := s.elevation.dataset(q.Get("dataset"))
	observation := &elevationCacheObservation{}
	ctx := context.WithValue(r.Context(), elevationCacheObservationKey{}, observation)
	values, err := s.elevation.lookup(ctx, []point{p}, dataset)
	if err != nil {
		writeOutboundError(w, err, "elevation")
		return
	}
	if observation.response.State != "" {
		setCacheHeaders(w, observation.response)
	}
	writeJSON(w, http.StatusOK, elevationResponse{Results: toResults(values), Dataset: dataset})
}

/* -- POST /elevation/batch -------------------------------------------- */

type batchRequest struct {
	Locations json.RawMessage `json:"locations"`
	Dataset   string          `json:"dataset"`
}

// handleElevationBatch looks up many points in one call.
//
// The client sends `locations` as "lat,lon|lat,lon|…". We split it into
// upstream-sized chunks and stitch the results back together in order, so the
// frontend never has to make one HTTP round trip per track point.
func (s *Server) handleElevationBatch(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	var payload batchRequest
	if err := decodeJSONBody(w, r, maxElevationBodyBytes, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(payload.Dataset) > 64 {
		writeError(w, http.StatusBadRequest, "dataset is too long")
		return
	}

	points, err := parseLocations(payload.Locations)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	dataset := s.elevation.dataset(payload.Dataset)
	results := make([]elevationResult, 0, len(points))
	observation := &elevationCacheObservation{}
	requestCtx, cancel := context.WithTimeout(r.Context(), maxElevationRequestDuration)
	defer cancel()
	ctx := context.WithValue(requestCtx, elevationCacheObservationKey{}, observation)

	for start := 0; start < len(points); start += maxLocationsPerRequest {
		end := min(start+maxLocationsPerRequest, len(points))
		values, err := s.elevation.lookup(ctx, points[start:end], dataset)
		if err != nil {
			writeOutboundError(w, err, "elevation")
			return
		}
		results = append(results, toResults(values)...)
	}

	if observation.response.State != "" {
		setCacheHeaders(w, observation.response)
	}
	writeJSON(w, http.StatusOK, elevationResponse{Results: results, Dataset: dataset})
}

/* -- Input parsing ---------------------------------------------------- */

func parsePoint(lat, lon string) (point, error) {
	latF, errLat := strconv.ParseFloat(strings.TrimSpace(lat), 64)
	lonF, errLon := strconv.ParseFloat(strings.TrimSpace(lon), 64)
	if errLat != nil || errLon != nil {
		return point{}, errors.New("lat and lon must be numbers")
	}
	if math.IsNaN(latF) || math.IsInf(latF, 0) || math.IsNaN(lonF) || math.IsInf(lonF, 0) || latF < -90 || latF > 90 || lonF < -180 || lonF > 180 {
		return point{}, errors.New("lat and lon are out of range")
	}
	return point{lat: latF, lon: lonF}, nil
}

// parseLocations accepts either the "lat,lon|lat,lon" string the frontend
// sends or a JSON array of ["lat,lon"] / [lat, lon] entries.
func parseLocations(raw json.RawMessage) ([]point, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing 'locations'")
	}

	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		if strings.Count(encoded, "|")+1 > maxElevationLocations {
			return nil, fmt.Errorf("locations must not exceed %d points", maxElevationLocations)
		}
		points := make([]point, 0, strings.Count(encoded, "|")+1)
		for _, chunk := range strings.Split(encoded, "|") {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			lat, lon, ok := strings.Cut(chunk, ",")
			if !ok {
				return nil, errors.New("each location must be \"lat,lon\"")
			}
			p, err := parsePoint(lat, lon)
			if err != nil {
				return nil, err
			}
			points = append(points, p)
		}
		if len(points) == 0 {
			return nil, errors.New("no locations supplied")
		}
		return points, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, errors.New("'locations' must be a string or an array")
	}
	points := make([]point, 0, min(maxElevationLocations, 256))
	for decoder.More() {
		if len(points) >= maxElevationLocations {
			return nil, fmt.Errorf("locations must not exceed %d points", maxElevationLocations)
		}
		var item json.RawMessage
		if decoder.Decode(&item) != nil {
			return nil, errors.New("invalid location array")
		}
		point, err := parseLocationItem(item)
		if err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, errors.New("invalid location array")
	}
	if len(points) == 0 {
		return nil, errors.New("no locations supplied")
	}
	return points, nil
}

func parseLocationItem(item json.RawMessage) (point, error) {
	var s string
	if err := json.Unmarshal(item, &s); err == nil {
		lat, lon, ok := strings.Cut(strings.TrimSpace(s), ",")
		if !ok {
			return point{}, errors.New("each location must be \"lat,lon\"")
		}
		return parsePoint(lat, lon)
	}
	var pair []float64
	if err := json.Unmarshal(item, &pair); err == nil && len(pair) == 2 {
		return parsePoint(strconv.FormatFloat(pair[0], 'g', -1, 64), strconv.FormatFloat(pair[1], 'g', -1, 64))
	}
	return point{}, errors.New("each location must be \"lat,lon\" or [lat, lon]")
}

/* -- Tile prefetch ---------------------------------------------------- */

type prefetchRequest struct {
	// Bbox is [south, west, north, east] — the map's own getBounds order.
	Bbox []float64 `json:"bbox"`
}

type prefetchResponse struct {
	// Enabled is false when the server is not in tile mode, so the UI can
	// stop asking rather than poll something that will never do anything.
	Enabled bool `json:"enabled"`
	prefetchProgress
}

// handlePrefetch warms the tile cache for the area the user is working in.
func (s *Server) handlePrefetch(w http.ResponseWriter, r *http.Request) {
	if s.elevation.tiles == nil {
		writeJSON(w, http.StatusOK, prefetchResponse{Enabled: false})
		return
	}

	var payload prefetchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "Malformed JSON body")
		return
	}
	if len(payload.Bbox) != 4 {
		writeError(w, http.StatusBadRequest, "bbox must be [south, west, north, east]")
		return
	}
	south, west, north, east := payload.Bbox[0], payload.Bbox[1], payload.Bbox[2], payload.Bbox[3]
	if _, err := parsePoint(fmt.Sprint(south), fmt.Sprint(west)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := parsePoint(fmt.Sprint(north), fmt.Sprint(east)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, prefetchResponse{
		Enabled:          true,
		prefetchProgress: s.elevation.tiles.startPrefetch(south, west, north, east),
	})
}

// handlePrefetchStatus is polled while a prefetch runs.
func (s *Server) handlePrefetchStatus(w http.ResponseWriter, r *http.Request) {
	if s.elevation.tiles == nil {
		writeJSON(w, http.StatusOK, prefetchResponse{Enabled: false})
		return
	}
	writeJSON(w, http.StatusOK, prefetchResponse{
		Enabled:          true,
		prefetchProgress: s.elevation.tiles.progress(),
	})
}
