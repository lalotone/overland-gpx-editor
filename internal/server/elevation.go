package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

/*
 * Elevation lookups.
 *
 * Two providers, one response shape. Open-Meteo is the default because it
 * needs no setup and works from anywhere; a self-hosted opentopodata instance
 * is better data (30 m postings against Copernicus 90 m — see docs/ACCURACY.md
 * on why that matters on a trail) and takes over as soon as its host is set.
 */

// Both upstreams cap a single request at 100 coordinates.
const maxLocationsPerRequest = 100

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
	// host is a self-hosted opentopodata-style service. Empty selects
	// Open-Meteo.
	host           string
	defaultDataset string
	client         *http.Client
}

func (e *elevationProxy) usesOpenMeteo() bool { return e.host == "" }

// dataset reports the effective dataset name for a request.
func (e *elevationProxy) dataset(requested string) string {
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
	if e.usesOpenMeteo() {
		return e.lookupOpenMeteo(ctx, points)
	}
	return e.lookupOpenTopoData(ctx, points, dataset)
}

func (e *elevationProxy) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxElevationBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("elevation service returned %d: %s",
			resp.StatusCode, strings.TrimSpace(truncate(string(body), 200)))
	}
	return body, nil
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
	values, err := s.elevation.lookup(r.Context(), []point{p}, dataset)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
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
	var payload batchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxElevationBodyBytes)).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "Malformed JSON body")
		return
	}

	points, err := parseLocations(payload.Locations)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	dataset := s.elevation.dataset(payload.Dataset)
	results := make([]elevationResult, 0, len(points))

	for start := 0; start < len(points); start += maxLocationsPerRequest {
		end := min(start+maxLocationsPerRequest, len(points))
		values, err := s.elevation.lookup(r.Context(), points[start:end], dataset)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		results = append(results, toResults(values)...)
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
	if latF < -90 || latF > 90 || lonF < -180 || lonF > 180 {
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
	if err := json.Unmarshal(raw, &encoded); err != nil {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, errors.New("'locations' must be a string or an array")
		}
		parts := make([]string, 0, len(items))
		for _, item := range items {
			part, err := parseLocationItem(item)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
		encoded = strings.Join(parts, "|")
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

func parseLocationItem(item json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(item, &s); err == nil {
		return strings.TrimSpace(s), nil
	}
	var pair []float64
	if err := json.Unmarshal(item, &pair); err == nil && len(pair) == 2 {
		return point{lat: pair[0], lon: pair[1]}.String(), nil
	}
	return "", errors.New("each location must be \"lat,lon\" or [lat, lon]")
}
