package server

import "testing"

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
