package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

type toolDefinition struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

var waypointMarkerIDs = []string{
	"generic", "fuel", "water", "camp", "food", "lodging", "parking", "repair", "medical",
	"viewpoint", "hazard", "roadblock", "ferry", "border", "restroom", "information", "picnic",
}

var toolDefinitions = []toolDefinition{
	{
		Name:        "get_view",
		Title:       "Inspect Overland view",
		Description: "Return the active Overland browser view, including map bounds, session overlays, marker catalog, planner route, editable track, waypoints, loaded and visible POIs, library files, terrain layers, and route status.",
		InputSchema: objectSchema(nil, nil),
		Annotations: map[string]any{"readOnlyHint": true, "idempotentHint": true},
	},
	{
		Name:        "plan_route",
		Title:       "Plan a route",
		Description: "Draw route control points. Requires planner mode; use switch_mode first. Overland routes between the points with the selected motorcycle profile. Use replace to start a route or append to extend it.",
		InputSchema: objectSchema(map[string]any{
			"points":  pointArraySchema(0, 100),
			"mode":    enumSchema("replace", "append"),
			"profile": enumSchema("road", "mixed", "trail"),
			"fitView": map[string]any{"type": "boolean", "description": "Fit the map to the supplied points."},
		}, []string{"points"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false},
	},
	{
		Name:        "draw_track",
		Title:       "Edit track geometry",
		Description: "Replace or append coordinates on the currently loaded track. Requires editor mode. The edit is undoable and does not save the GPX file until the user explicitly saves it.",
		InputSchema: objectSchema(map[string]any{
			"points":  pointArraySchema(1, 6000),
			"mode":    enumSchema("replace", "append"),
			"fitView": map[string]any{"type": "boolean", "description": "Fit the map to the edited track."},
		}, []string{"points"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true},
	},
	{
		Name:        "set_waypoints",
		Title:       "Set GPX waypoints",
		Description: "Replace or append named GPX waypoints in the matching planner or editor mode. Catalog marker IDs map to GPX symbols; arbitrary sym values remain supported for imported-device compatibility.",
		InputSchema: objectSchema(map[string]any{
			"target": enumSchema("planner", "track"),
			"mode":   enumSchema("replace", "append"),
			"waypoints": map[string]any{
				"type": "array", "maxItems": 1000,
				"items": semanticWaypointSchema(),
			},
		}, []string{"target", "waypoints"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true},
	},
	{
		Name:        "draw_map_track",
		Title:       "Draw session map track",
		Description: "Replace, append, or clear a session-only track overlay on the active planner, editor, or Explore map without switching modes. It is never written to GPX.",
		InputSchema: objectSchema(map[string]any{
			"points":  pointArraySchema(0, 6000),
			"mode":    enumSchema("replace", "append"),
			"fitView": map[string]any{"type": "boolean", "description": "Fit the active map to the supplied track."},
		}, []string{"points"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false},
	},
	{
		Name:        "set_map_markers",
		Title:       "Set session map markers",
		Description: "Replace, append, or clear typed session-only markers on the active planner, editor, or Explore map without switching modes. They are never written to GPX.",
		InputSchema: objectSchema(map[string]any{
			"mode": enumSchema("replace", "append"),
			"markers": map[string]any{
				"type": "array", "maxItems": 1000,
				"items": mapMarkerSchema(),
			},
			"fitView": map[string]any{"type": "boolean", "description": "Fit the active map to the supplied markers."},
		}, []string{"markers"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false},
	},
	{
		Name:        "switch_mode",
		Title:       "Switch map mode",
		Description: "Switch the active browser to planner, editor, or Explore mode. Editor mode requires an open track. Session map overlays remain visible.",
		InputSchema: objectSchema(map[string]any{
			"mode": enumSchema("planner", "editor", "explore"),
		}, []string{"mode"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
	},
	{
		Name:        "set_map_view",
		Title:       "Move map view",
		Description: "Pan the active planning, track-edit, or Explore map to a latitude, longitude, and optional zoom level.",
		InputSchema: objectSchema(map[string]any{
			"lat":  map[string]any{"type": "number", "minimum": -90, "maximum": 90},
			"lon":  map[string]any{"type": "number", "minimum": -180, "maximum": 180},
			"zoom": map[string]any{"type": "number", "minimum": 1, "maximum": 20},
		}, []string{"lat", "lon"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
	},
	{
		Name:        "open_track",
		Title:       "Open library track",
		Description: "Open an exact .gpx filename from the Overland library in track-edit mode. Refuses while the current track has unsaved edits.",
		InputSchema: objectSchema(map[string]any{
			"filename": map[string]any{"type": "string", "maxLength": 255, "pattern": `^(?!\.)(?!.*\u0000)[^/\\]+\.[gG][pP][xX]$`},
		}, []string{"filename"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false},
	},
	{
		Name:        "select_track",
		Title:       "Select GPX track",
		Description: "Select one track by zero-based index when the open GPX file contains multiple tracks.",
		InputSchema: objectSchema(map[string]any{
			"index": map[string]any{"type": "integer", "minimum": 0},
		}, []string{"index"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
	},
	{
		Name:        "load_pois",
		Title:       "Load planning POIs",
		Description: "Load fuel, water, or campsite POIs into the current planning viewport or around the current track. Results become available through get_view and overland://view.",
		InputSchema: objectSchema(map[string]any{
			"kinds": map[string]any{"type": "array", "minItems": 1, "maxItems": 3, "uniqueItems": true, "items": enumSchema("fuel", "water", "camp")},
			"scope": enumSchema("current_view", "current_track"),
		}, []string{"kinds", "scope"}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false},
	},
}

type pointArgument struct {
	Lat       *float64 `json:"lat"`
	Lon       *float64 `json:"lon"`
	Elevation *float64 `json:"elevation,omitempty"`
}

type waypointArgument struct {
	Lat       *float64 `json:"lat"`
	Lon       *float64 `json:"lon"`
	Elevation *float64 `json:"elevation,omitempty"`
	Name      string   `json:"name,omitempty"`
	Desc      string   `json:"desc,omitempty"`
	Marker    *string  `json:"marker,omitempty"`
	Sym       *string  `json:"sym,omitempty"`
	Type      string   `json:"type,omitempty"`
}

type mapMarkerArgument struct {
	Lat       *float64 `json:"lat"`
	Lon       *float64 `json:"lon"`
	Elevation *float64 `json:"elevation,omitempty"`
	Name      string   `json:"name,omitempty"`
	Desc      string   `json:"desc,omitempty"`
	Marker    string   `json:"marker"`
}

func validateToolArguments(name string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	switch name {
	case "get_view":
		var args struct{}
		return marshalValidated(args, decodeStrict(raw, &args))
	case "plan_route":
		return validatePlanRoute(raw)
	case "draw_track":
		return validateDrawTrack(raw)
	case "set_waypoints":
		return validateSetWaypoints(raw)
	case "draw_map_track":
		return validateDrawMapTrack(raw)
	case "set_map_markers":
		return validateSetMapMarkers(raw)
	case "switch_mode":
		return validateSwitchMode(raw)
	case "set_map_view":
		return validateSetMapView(raw)
	case "open_track":
		return validateOpenTrack(raw)
	case "select_track":
		return validateSelectTrack(raw)
	case "load_pois":
		return validateLoadPOIs(raw)
	default:
		return nil, errors.New("unknown tool")
	}
}

func validatePlanRoute(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Points  *[]pointArgument `json:"points"`
		Mode    string           `json:"mode,omitempty"`
		Profile string           `json:"profile,omitempty"`
		FitView *bool            `json:"fitView,omitempty"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if args.Points == nil {
		return nil, errors.New("points is required")
	}
	if len(*args.Points) > 100 {
		return nil, errors.New("points must contain at most 100 route controls")
	}
	if err := validatePoints(*args.Points); err != nil {
		return nil, err
	}
	if args.Mode == "" {
		args.Mode = "replace"
	}
	if args.Mode != "replace" && args.Mode != "append" {
		return nil, errors.New("mode must be replace or append")
	}
	if args.Profile != "" && args.Profile != "road" && args.Profile != "mixed" && args.Profile != "trail" {
		return nil, errors.New("profile must be road, mixed, or trail")
	}
	return marshalValidated(args, nil)
}

func validateDrawTrack(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Points  *[]pointArgument `json:"points"`
		Mode    string           `json:"mode,omitempty"`
		FitView *bool            `json:"fitView,omitempty"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if args.Points == nil || len(*args.Points) == 0 || len(*args.Points) > 6000 {
		return nil, errors.New("points must contain 1 to 6000 coordinates")
	}
	if err := validatePoints(*args.Points); err != nil {
		return nil, err
	}
	if args.Mode == "" {
		args.Mode = "replace"
	}
	if args.Mode != "replace" && args.Mode != "append" {
		return nil, errors.New("mode must be replace or append")
	}
	return marshalValidated(args, nil)
}

func validateSetWaypoints(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Target    string              `json:"target"`
		Mode      string              `json:"mode,omitempty"`
		Waypoints *[]waypointArgument `json:"waypoints"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if args.Target != "planner" && args.Target != "track" {
		return nil, errors.New("target must be planner or track")
	}
	if args.Mode == "" {
		args.Mode = "replace"
	}
	if args.Mode != "replace" && args.Mode != "append" {
		return nil, errors.New("mode must be replace or append")
	}
	if args.Waypoints == nil {
		return nil, errors.New("waypoints is required")
	}
	if len(*args.Waypoints) > 1000 {
		return nil, errors.New("waypoints must contain at most 1000 items")
	}
	for i, waypoint := range *args.Waypoints {
		if err := validateCoordinate(waypoint.Lat, waypoint.Lon, waypoint.Elevation); err != nil {
			return nil, fmt.Errorf("waypoint %d: %w", i, err)
		}
		if utf8.RuneCountInString(waypoint.Name) > 200 || utf8.RuneCountInString(waypoint.Desc) > 2000 ||
			(waypoint.Sym != nil && utf8.RuneCountInString(*waypoint.Sym) > 100) || utf8.RuneCountInString(waypoint.Type) > 100 {
			return nil, fmt.Errorf("waypoint %d contains text that is too long", i)
		}
		if waypoint.Marker != nil && !validWaypointMarker(*waypoint.Marker) {
			return nil, fmt.Errorf("waypoint %d has an unknown marker", i)
		}
		if waypoint.Marker != nil && waypoint.Sym != nil {
			return nil, fmt.Errorf("waypoint %d cannot set both marker and sym", i)
		}
	}
	return marshalValidated(args, nil)
}

func validateDrawMapTrack(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Points  *[]pointArgument `json:"points"`
		Mode    string           `json:"mode,omitempty"`
		FitView *bool            `json:"fitView,omitempty"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if args.Points == nil || len(*args.Points) > 6000 {
		return nil, errors.New("points must contain at most 6000 coordinates")
	}
	if err := validatePoints(*args.Points); err != nil {
		return nil, err
	}
	if args.Mode == "" {
		args.Mode = "replace"
	}
	if args.Mode != "replace" && args.Mode != "append" {
		return nil, errors.New("mode must be replace or append")
	}
	return marshalValidated(args, nil)
}

func validateSetMapMarkers(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Mode    string               `json:"mode,omitempty"`
		Markers *[]mapMarkerArgument `json:"markers"`
		FitView *bool                `json:"fitView,omitempty"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if args.Mode == "" {
		args.Mode = "replace"
	}
	if args.Mode != "replace" && args.Mode != "append" {
		return nil, errors.New("mode must be replace or append")
	}
	if args.Markers == nil || len(*args.Markers) > 1000 {
		return nil, errors.New("markers must contain at most 1000 items")
	}
	for i, marker := range *args.Markers {
		if err := validateCoordinate(marker.Lat, marker.Lon, marker.Elevation); err != nil {
			return nil, fmt.Errorf("marker %d: %w", i, err)
		}
		if !validWaypointMarker(marker.Marker) {
			return nil, fmt.Errorf("marker %d has an unknown marker type", i)
		}
		if utf8.RuneCountInString(marker.Name) > 200 || utf8.RuneCountInString(marker.Desc) > 2000 {
			return nil, fmt.Errorf("marker %d contains text that is too long", i)
		}
	}
	return marshalValidated(args, nil)
}

func validateSwitchMode(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Mode string `json:"mode"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if args.Mode != "planner" && args.Mode != "editor" && args.Mode != "explore" {
		return nil, errors.New("mode must be planner, editor, or explore")
	}
	return marshalValidated(args, nil)
}

func validateSetMapView(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Lat  *float64 `json:"lat"`
		Lon  *float64 `json:"lon"`
		Zoom *float64 `json:"zoom,omitempty"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if err := validateCoordinate(args.Lat, args.Lon, nil); err != nil {
		return nil, err
	}
	if args.Zoom != nil && (!finite(*args.Zoom) || *args.Zoom < 1 || *args.Zoom > 20) {
		return nil, errors.New("zoom must be between 1 and 20")
	}
	return marshalValidated(args, nil)
}

func validateOpenTrack(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Filename string `json:"filename"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Filename) == 0 || len(args.Filename) > 255 || strings.ContainsRune(args.Filename, 0) || strings.HasPrefix(args.Filename, ".") ||
		strings.ContainsAny(args.Filename, `/\`) || filepath.Base(args.Filename) != args.Filename ||
		!strings.EqualFold(filepath.Ext(args.Filename), ".gpx") {
		return nil, errors.New("filename must be a bare .gpx filename")
	}
	return marshalValidated(args, nil)
}

func validateSelectTrack(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Index *int `json:"index"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if args.Index == nil || *args.Index < 0 {
		return nil, errors.New("index must not be negative")
	}
	return marshalValidated(args, nil)
}

func validateLoadPOIs(raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Kinds []string `json:"kinds"`
		Scope string   `json:"scope"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Kinds) == 0 || len(args.Kinds) > 3 {
		return nil, errors.New("kinds must contain 1 to 3 POI kinds")
	}
	seen := make(map[string]struct{}, len(args.Kinds))
	for _, kind := range args.Kinds {
		if kind != "fuel" && kind != "water" && kind != "camp" {
			return nil, errors.New("POI kind must be fuel, water, or camp")
		}
		if _, exists := seen[kind]; exists {
			return nil, errors.New("POI kinds must be unique")
		}
		seen[kind] = struct{}{}
	}
	if args.Scope != "current_view" && args.Scope != "current_track" {
		return nil, errors.New("scope must be current_view or current_track")
	}
	return marshalValidated(args, nil)
}

func validatePoints(points []pointArgument) error {
	for i, point := range points {
		if err := validateCoordinate(point.Lat, point.Lon, point.Elevation); err != nil {
			return fmt.Errorf("point %d: %w", i, err)
		}
	}
	return nil
}

func validateCoordinate(lat, lon, elevation *float64) error {
	if lat == nil || lon == nil {
		return errors.New("latitude and longitude are required")
	}
	if !finite(*lat) || *lat < -90 || *lat > 90 || !finite(*lon) || *lon < -180 || *lon > 180 {
		return errors.New("latitude or longitude is out of range")
	}
	if elevation != nil && !finite(*elevation) {
		return errors.New("elevation must be finite")
	}
	return nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func decodeStrict(raw json.RawMessage, target any) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil || containsJSONNull(value) {
		return errors.New("invalid tool arguments")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid tool arguments")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid tool arguments")
	}
	return nil
}

func containsJSONNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case []any:
		return slices.ContainsFunc(value, containsJSONNull)
	case map[string]any:
		for _, item := range value {
			if containsJSONNull(item) {
				return true
			}
		}
	}
	return false
}

func marshalValidated(value any, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func pointArraySchema(minItems, maxItems int) map[string]any {
	return map[string]any{
		"type": "array", "minItems": minItems, "maxItems": maxItems,
		"items": objectSchema(map[string]any{
			"lat":       map[string]any{"type": "number", "minimum": -90, "maximum": 90},
			"lon":       map[string]any{"type": "number", "minimum": -180, "maximum": 180},
			"elevation": map[string]any{"type": "number"},
		}, []string{"lat", "lon"}),
	}
}

func semanticWaypointSchema() map[string]any {
	return objectSchema(map[string]any{
		"lat":       map[string]any{"type": "number", "minimum": -90, "maximum": 90},
		"lon":       map[string]any{"type": "number", "minimum": -180, "maximum": 180},
		"elevation": map[string]any{"type": "number"},
		"name":      map[string]any{"type": "string", "maxLength": 200},
		"desc":      map[string]any{"type": "string", "maxLength": 2000},
		"marker":    enumSchema(waypointMarkerIDs...),
		"sym":       map[string]any{"type": "string", "maxLength": 100},
		"type":      map[string]any{"type": "string", "maxLength": 100},
	}, []string{"lat", "lon"})
}

func mapMarkerSchema() map[string]any {
	return objectSchema(map[string]any{
		"lat":       map[string]any{"type": "number", "minimum": -90, "maximum": 90},
		"lon":       map[string]any{"type": "number", "minimum": -180, "maximum": 180},
		"elevation": map[string]any{"type": "number"},
		"name":      map[string]any{"type": "string", "maxLength": 200},
		"desc":      map[string]any{"type": "string", "maxLength": 2000},
		"marker":    enumSchema(waypointMarkerIDs...),
	}, []string{"lat", "lon", "marker"})
}

func validWaypointMarker(value string) bool {
	return slices.Contains(waypointMarkerIDs, value)
}

func enumSchema(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}
