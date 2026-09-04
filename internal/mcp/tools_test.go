package mcp

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestToolDefinitionsIncludeMapControlTools(t *testing.T) {
	want := []string{
		"get_view", "plan_route", "draw_track", "set_waypoints", "draw_map_track",
		"set_map_markers", "switch_mode", "set_map_view", "open_track", "select_track", "load_pois",
	}
	for _, name := range want {
		if !slices.ContainsFunc(toolDefinitions, func(definition toolDefinition) bool {
			return definition.Name == name
		}) {
			t.Errorf("tool %q is not registered", name)
		}
	}
	if len(toolDefinitions) != len(want) {
		t.Errorf("tool count = %d, want %d", len(toolDefinitions), len(want))
	}
}

func TestWaypointMarkerIDs(t *testing.T) {
	want := []string{
		"generic", "fuel", "water", "camp", "food", "lodging", "parking", "repair", "medical",
		"viewpoint", "hazard", "roadblock", "ferry", "border", "restroom", "information", "picnic",
	}
	if !slices.Equal(waypointMarkerIDs, want) {
		t.Fatalf("marker IDs = %v, want %v", waypointMarkerIDs, want)
	}
}

func TestValidateMapOverlayArguments(t *testing.T) {
	validated, err := validateToolArguments("set_map_markers", json.RawMessage(`{
		"markers":[{"lat":42.8467,"lon":-2.6726,"marker":"fuel","name":"Fill up"}],
		"fitView":false
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var markers struct {
		Mode    string              `json:"mode"`
		Markers []mapMarkerArgument `json:"markers"`
		FitView *bool               `json:"fitView"`
	}
	if err := json.Unmarshal(validated, &markers); err != nil {
		t.Fatal(err)
	}
	if markers.Mode != "replace" || len(markers.Markers) != 1 || markers.Markers[0].Marker != "fuel" {
		t.Fatalf("validated marker arguments = %#v", markers)
	}
	if markers.FitView == nil || *markers.FitView {
		t.Fatalf("fitView = %v, want false", markers.FitView)
	}

	if _, err := validateToolArguments("draw_map_track", json.RawMessage(`{"points":[]}`)); err != nil {
		t.Fatalf("empty replacement should clear the session track: %v", err)
	}
	if _, err := validateToolArguments("set_map_markers", json.RawMessage(`{"markers":[{"lat":42,"lon":-2,"marker":"script"}]}`)); err == nil {
		t.Fatal("unknown marker type was accepted")
	}
}

func TestValidateSemanticWaypointMarker(t *testing.T) {
	if _, err := validateToolArguments("set_waypoints", json.RawMessage(`{
		"target":"planner","waypoints":[{"lat":42,"lon":-2,"marker":"camp"}]
	}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := validateToolArguments("set_waypoints", json.RawMessage(`{
		"target":"planner","waypoints":[{"lat":42,"lon":-2,"marker":"camp","sym":"Tent"}]
	}`)); err == nil {
		t.Fatal("marker and sym were accepted together")
	}
	if _, err := validateToolArguments("set_waypoints", json.RawMessage(`{
		"target":"planner","waypoints":[{"lat":42,"lon":-2,"marker":""}]
	}`)); err == nil {
		t.Fatal("empty marker was accepted")
	}
}

func TestValidateSwitchMode(t *testing.T) {
	for _, mode := range []string{"planner", "editor", "explore"} {
		if _, err := validateToolArguments("switch_mode", json.RawMessage(`{"mode":"`+mode+`"}`)); err != nil {
			t.Errorf("mode %q: %v", mode, err)
		}
	}
	if _, err := validateToolArguments("switch_mode", json.RawMessage(`{"mode":"welcome"}`)); err == nil {
		t.Fatal("non-map mode was accepted")
	}
}

func TestValidateToolArgumentsRejectsNull(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args string
	}{
		{name: "mode", tool: "draw_map_track", args: `{"points":[],"mode":null}`},
		{name: "fit view", tool: "draw_map_track", args: `{"points":[],"fitView":null}`},
		{name: "marker", tool: "set_waypoints", args: `{"target":"planner","waypoints":[{"lat":42,"lon":-2,"marker":null}]}`},
		{name: "symbol", tool: "set_waypoints", args: `{"target":"planner","waypoints":[{"lat":42,"lon":-2,"sym":null}]}`},
		{name: "name", tool: "set_map_markers", args: `{"markers":[{"lat":42,"lon":-2,"marker":"camp","name":null}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateToolArguments(test.tool, json.RawMessage(test.args)); err == nil {
				t.Fatal("null argument was accepted")
			}
		})
	}
}
