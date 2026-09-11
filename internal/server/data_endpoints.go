package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type coordinate struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

func (c coordinate) valid() bool {
	return !math.IsNaN(c.Lat) && !math.IsInf(c.Lat, 0) && !math.IsNaN(c.Lon) && !math.IsInf(c.Lon, 0) &&
		c.Lat >= -90 && c.Lat <= 90 && c.Lon >= -180 && c.Lon <= 180
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	if contentType := r.Header.Get("Content-Type"); contentType != "" && !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("malformed JSON body")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("JSON body must contain one value")
	}
	return nil
}

func providerEndpoint(base *url.URL, suffix string) string {
	copyURL := *base
	copyURL.Path = strings.TrimRight(copyURL.Path, "/") + suffix
	copyURL.RawQuery = ""
	return copyURL.String()
}

func validateFuelResponse(body []byte) error {
	var payload struct {
		Stations  json.RawMessage `json:"ListaEESSPrecio"`
		Published json.RawMessage `json:"Fecha"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Stations) == 0 {
		return errors.New("fuel provider returned an invalid snapshot")
	}
	if len(payload.Published) > 0 {
		var published string
		if json.Unmarshal(payload.Published, &published) != nil {
			return errors.New("fuel provider returned an invalid publication date")
		}
	}
	var stations []map[string]json.RawMessage
	if json.Unmarshal(payload.Stations, &stations) != nil || stations == nil {
		return errors.New("fuel provider returned an invalid station list")
	}
	for _, station := range stations {
		id, ok := fuelString(station, "IDEESS")
		latRaw, latOK := fuelString(station, "Latitud")
		lonRaw, lonOK := fuelString(station, "Longitud (WGS84)")
		lat, latErr := parseSpanishNumber(latRaw)
		lon, lonErr := parseSpanishNumber(lonRaw)
		if !ok || strings.TrimSpace(id) == "" || !latOK || !lonOK || latErr != nil || lonErr != nil || !(coordinate{Lat: lat, Lon: lon}).valid() {
			return errors.New("fuel provider returned an invalid station")
		}
		for _, field := range []string{"Rótulo", "Dirección", "Municipio", "Horario"} {
			if raw, exists := station[field]; exists {
				var value string
				if json.Unmarshal(raw, &value) != nil {
					return errors.New("fuel provider returned an invalid station field")
				}
			}
		}
		for _, field := range []string{"Precio Gasolina 95 E5", "Precio Gasolina 98 E5", "Precio Gasoleo A", "Precio Gasoleo Premium"} {
			if raw, exists := station[field]; exists {
				var value string
				if json.Unmarshal(raw, &value) != nil {
					return errors.New("fuel provider returned an invalid price")
				}
				if strings.TrimSpace(value) != "" {
					price, err := parseSpanishNumber(value)
					if err != nil || price <= 0 {
						return errors.New("fuel provider returned an invalid price")
					}
				}
			}
		}
	}
	return nil
}

func fuelString(station map[string]json.RawMessage, field string) (string, bool) {
	raw, ok := station[field]
	if !ok {
		return "", false
	}
	var value string
	err := json.Unmarshal(raw, &value)
	return value, err == nil
}

func parseSpanishNumber(raw string) (float64, error) {
	return strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(raw), ",", "."), 64)
}

func validatePlaceResponse(body []byte) error {
	var places []struct {
		PlaceID     int64  `json:"place_id"`
		DisplayName string `json:"display_name"`
		Lat         string `json:"lat"`
		Lon         string `json:"lon"`
	}
	if json.Unmarshal(body, &places) != nil || places == nil {
		return errors.New("place provider returned an invalid result list")
	}
	for _, place := range places {
		lat, latErr := strconv.ParseFloat(place.Lat, 64)
		lon, lonErr := strconv.ParseFloat(place.Lon, 64)
		if place.PlaceID <= 0 || strings.TrimSpace(place.DisplayName) == "" || latErr != nil || lonErr != nil || !(coordinate{Lat: lat, Lon: lon}).valid() {
			return errors.New("place provider returned an invalid result")
		}
	}
	return nil
}

func validatePOIResponse(body []byte) error {
	var payload struct {
		Elements json.RawMessage `json:"elements"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Elements) == 0 {
		return errors.New("POI provider returned an invalid response")
	}
	var elements []struct {
		Type   string   `json:"type"`
		ID     float64  `json:"id"`
		Lat    *float64 `json:"lat"`
		Lon    *float64 `json:"lon"`
		Center *struct {
			Lat *float64 `json:"lat"`
			Lon *float64 `json:"lon"`
		} `json:"center"`
		Tags map[string]json.RawMessage `json:"tags"`
	}
	if json.Unmarshal(payload.Elements, &elements) != nil || elements == nil {
		return errors.New("POI provider returned an invalid element list")
	}
	for _, element := range elements {
		if (element.Type != "node" && element.Type != "way" && element.Type != "relation") || element.ID <= 0 || math.Trunc(element.ID) != element.ID {
			return errors.New("POI provider returned an invalid element")
		}
		if (element.Lat == nil) != (element.Lon == nil) || (element.Lat != nil && !(coordinate{Lat: *element.Lat, Lon: *element.Lon}).valid()) {
			return errors.New("POI provider returned invalid coordinates")
		}
		if element.Center != nil && (element.Center.Lat == nil || element.Center.Lon == nil || !(coordinate{Lat: *element.Center.Lat, Lon: *element.Center.Lon}).valid()) {
			return errors.New("POI provider returned invalid center coordinates")
		}
		if element.Lat == nil && element.Center == nil {
			return errors.New("POI provider returned an element without geometry")
		}
		for _, field := range []string{"name", "brand", "operator"} {
			if raw, ok := element.Tags[field]; ok {
				var value string
				if json.Unmarshal(raw, &value) != nil {
					return errors.New("POI provider returned invalid tags")
				}
			}
		}
	}
	return nil
}

func (s *Server) handleFuel(w http.ResponseWriter, r *http.Request) {
	p := s.providers["fuel"]
	response, err := s.outbound.do(r.Context(), cachedRequest{
		policy: p, method: http.MethodGet, url: p.baseURL.String(), params: "national-snapshot", cacheable: true,
		headers:  map[string]string{"Accept": "application/json"},
		validate: validateFuelResponse,
	})
	if err != nil {
		writeOutboundError(w, err, p.scope)
		return
	}
	writeCachedResponse(w, response)
}

var languagePattern = regexp.MustCompile(`^[A-Za-z]{1,8}(?:[-_][A-Za-z0-9]{1,8}){0,3}$`)

func normalizeSearchQuery(value string) (string, error) {
	normalized := strings.Join(strings.Fields(value), " ")
	if normalized == "" {
		return "", errors.New("q is required")
	}
	if len(normalized) > 200 {
		return "", errors.New("q is too long")
	}
	return normalized, nil
}

func (s *Server) handlePlaceSearch(w http.ResponseWriter, r *http.Request) {
	query, err := normalizeSearchQuery(r.URL.Query().Get("q"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	language := strings.TrimSpace(r.URL.Query().Get("language"))
	if language == "" {
		language = "en"
	}
	if len(language) > 32 || !languagePattern.MatchString(language) {
		writeError(w, http.StatusBadRequest, "invalid language")
		return
	}
	p := s.providers["places"]
	values := url.Values{"q": {query}, "format": {"jsonv2"}, "limit": {"5"}}
	endpoint := providerEndpoint(p.baseURL, "/search") + "?" + values.Encode()
	response, err := s.outbound.do(r.Context(), cachedRequest{
		policy: p, method: http.MethodGet, url: endpoint,
		params: query, language: language, cacheable: true,
		headers:  map[string]string{"Accept": "application/json", "Accept-Language": language},
		validate: validatePlaceResponse,
	})
	if err != nil {
		writeOutboundError(w, err, p.scope)
		return
	}
	writeCachedResponse(w, response)
}

type bbox struct {
	South float64 `json:"south"`
	West  float64 `json:"west"`
	North float64 `json:"north"`
	East  float64 `json:"east"`
}

func (b bbox) validate(maxSpanKM float64, allowAntimeridian bool) error {
	for _, c := range []coordinate{{b.South, b.West}, {b.North, b.East}} {
		if !c.valid() {
			return errors.New("bbox coordinates are out of range")
		}
	}
	if b.South >= b.North || (!allowAntimeridian && b.West >= b.East) || (allowAntimeridian && b.West == b.East) {
		return errors.New("bbox bounds are not ordered")
	}
	widthDegrees := b.East - b.West
	if widthDegrees < 0 {
		widthDegrees += 360
	}
	mid := (b.South + b.North) / 2 * math.Pi / 180
	width := widthDegrees * 111.195 * math.Max(0.01, math.Cos(mid))
	height := (b.North - b.South) * 111.195
	if width > maxSpanKM || height > maxSpanKM {
		return fmt.Errorf("bbox span must not exceed %.0f km", maxSpanKM)
	}
	return nil
}

type poiRequest struct {
	Kind string `json:"kind"`
	BBox bbox   `json:"bbox"`
}

func (s *Server) handlePOISearch(w http.ResponseWriter, r *http.Request) {
	var payload poiRequest
	if err := decodeJSONBody(w, r, 16<<10, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request, err := buildPOIRequest(s.providers["pois"], payload.Kind, payload.BBox)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	response, err := s.outbound.do(r.Context(), request)
	if err != nil {
		writeOutboundError(w, err, request.policy.scope)
		return
	}
	writeCachedResponse(w, response)
}

func buildPOIRequest(policy *providerPolicy, kind string, bounds bbox) (cachedRequest, error) {
	selector := map[string]string{
		"fuel":  `nwr["amenity"="fuel"]`,
		"water": `nwr["amenity"="drinking_water"]`,
		"camp":  `nwr["tourism"~"^(camp_site|caravan_site)$"]`,
	}[kind]
	if selector == "" {
		return cachedRequest{}, errors.New("kind must be fuel, water, or camp")
	}
	if err := bounds.validate(250, false); err != nil {
		return cachedRequest{}, err
	}
	area := fmt.Sprintf("%.5f,%.5f,%.5f,%.5f", bounds.South, bounds.West, bounds.North, bounds.East)
	query := fmt.Sprintf("[out:json][timeout:30];%s(%s);out center 400;", selector, area)
	form := url.Values{"data": {query}}.Encode()
	return cachedRequest{
		policy: policy, method: http.MethodPost, url: policy.baseURL.String(), params: kind + ":" + area,
		body: []byte(form), cacheable: true,
		headers:  map[string]string{"Accept": "application/json", "Content-Type": "application/x-www-form-urlencoded"},
		validate: validatePOIResponse,
	}, nil
}

func validatePoints(points []coordinate, minPoints, maxPoints int) error {
	if len(points) < minPoints || len(points) > maxPoints {
		return fmt.Errorf("points must contain %d..%d coordinates", minPoints, maxPoints)
	}
	for _, point := range points {
		if !point.valid() {
			return errors.New("point coordinates are out of range")
		}
	}
	return nil
}

func haversineMeters(a, b coordinate) float64 {
	const radius = 6371008.8
	lat1, lat2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dLat, dLon := lat2-lat1, (b.Lon-a.Lon)*math.Pi/180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * radius * math.Asin(math.Sqrt(h))
}
