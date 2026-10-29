package server

import (
	"net/url"
	"strings"
	"testing"
)

func TestProviderResponseValidators(t *testing.T) {
	tests := []struct {
		name     string
		validate func([]byte) error
		valid    string
		invalid  string
	}{
		{
			name: "fuel", validate: validateFuelResponse,
			valid:   `{"Fecha":"30/08/2026","ListaEESSPrecio":[{"IDEESS":"1","Latitud":"40,4","Longitud (WGS84)":"-3,7","Rótulo":"Brand","Precio Gasolina 95 E5":"1,599"}]}`,
			invalid: `{"Fecha":7,"ListaEESSPrecio":[{"IDEESS":"1","Latitud":"40,4","Longitud (WGS84)":"-3,7"}]}`,
		},
		{
			name: "places", validate: validatePlaceResponse,
			valid:   `[{"place_id":1,"display_name":"Madrid","lat":"40.4","lon":"-3.7"}]`,
			invalid: `[{"display_name":"Madrid","lat":"40.4","lon":"-3.7"}]`,
		},
		{
			name: "overpass", validate: validatePOIResponse,
			valid:   `{"elements":[{"type":"node","id":1,"lat":40.4,"lon":-3.7,"tags":{"name":"Fuel"}}]}`,
			invalid: `{"elements":[{"type":"node","id":1,"lat":140.4,"lon":-3.7}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.validate([]byte(tt.valid)); err != nil {
				t.Fatalf("valid response rejected: %v", err)
			}
			if err := tt.validate([]byte(tt.invalid)); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestAroundPOIRequestIsOneBoundedUnion(t *testing.T) {
	s := newTestServer(t)
	policy := s.providers["pois"]
	request, err := buildPOIRequest(policy, "around", bbox{South: 42.4, West: -0.8, North: 42.8, East: -0.3})
	if err != nil {
		t.Fatal(err)
	}
	query, err := url.ParseQuery(string(request.body))
	if err != nil {
		t.Fatal(err)
	}
	data := query.Get("data")
	for _, want := range []string{
		"[bbox:42.40000,-0.80000,42.80000,-0.30000]", `"natural"="spring"`, "alpine_hut", "motorcycle_repair",
		`["shelter_type"!="public_transport"]`, "out center 1500;",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("around query lacks %q: %s", want, data)
		}
	}
	if !request.cacheable || request.params != "around:42.40000,-0.80000,42.80000,-0.30000" {
		t.Errorf("around request must be cached per area, params %q", request.params)
	}
	// A regional sweep is refused even though the single-kind limit allows it.
	if _, err := buildPOIRequest(policy, "around", bbox{South: 42, West: -1, North: 43, East: 0}); err == nil {
		t.Error("around accepted a 110 km area")
	}
	if _, err := buildPOIRequest(policy, "fuel", bbox{South: 42, West: -1, North: 43, East: 0}); err != nil {
		t.Errorf("single-kind limit changed: %v", err)
	}
	if _, err := buildPOIRequest(policy, "everything", bbox{South: 42, West: -1, North: 42.1, East: -0.9}); err == nil {
		t.Error("an unknown kind was accepted")
	}
}
