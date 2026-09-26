package server

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFuelSnapshotAppliesOnlyNearSpain(t *testing.T) {
	cases := []struct {
		name string
		area *bbox
		want bool
	}{
		{"route pack without bounds", nil, true},
		{"Melilla", &bbox{South: 35.26, West: -2.97, North: 35.32, East: -2.91}, true},
		{"Canarias", &bbox{South: 28.0, West: -16.9, North: 28.6, East: -16.1}, true},
		{"Andorra", &bbox{South: 42.43, West: 1.41, North: 42.66, East: 1.79}, true}, // inside the envelope; harmless
		{"Utrecht", &bbox{South: 51.86, West: 4.79, North: 52.31, East: 5.63}, false},
		{"Morocco interior", &bbox{South: 31.0, West: -8.0, North: 33.0, East: -5.0}, false},
	}
	for _, tc := range cases {
		if got := fuelSnapshotApplies(tc.area); got != tc.want {
			t.Errorf("%s: fuelSnapshotApplies = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPackFailureReasonIsShortAndURLFree(t *testing.T) {
	if got := packFailureReason(nil); got != "" {
		t.Fatalf("nil error reason = %q", got)
	}
	if got := packFailureReason(context.DeadlineExceeded); got != "the request timed out" {
		t.Fatalf("deadline reason = %q", got)
	}
	leaky := errors.New(`Get "https://overpass-api.de/api/interpreter?data=bbox(1,2,3,4)": dial tcp: connection refused`)
	if got := packFailureReason(leaky); strings.Contains(got, "overpass") || strings.Contains(got, "bbox") {
		t.Fatalf("reason leaks the request: %q", got)
	}
	long := errors.New(strings.Repeat("x", 500))
	if got := packFailureReason(long); len(got) > 170 {
		t.Fatalf("reason not truncated: %d bytes", len(got))
	}
	if got := packFailureReason(errors.New("upstream temporarily unavailable")); got != "upstream temporarily unavailable" {
		t.Fatalf("plain reason changed: %q", got)
	}
}

func TestNoteResourceErrorKeepsLatestReason(t *testing.T) {
	p := &packManifest{}
	updatePackResource(p, packResourceCampsites, true, 0, 0)
	noteResourceError(p, packResourceCampsites, errors.New("upstream temporarily unavailable"))
	if got := p.Resources[packResourceCampsites]; got.Failed != 1 || got.Error != "upstream temporarily unavailable" {
		t.Fatalf("resource progress = %+v", got)
	}
}

func TestRoutingRegionDisplayName(t *testing.T) {
	cases := map[[2]string]string{
		{"andorra", "Andorra"}:                                 "Andorra",
		{"us/district-of-columbia", "us/district-of-columbia"}: "District of Columbia",
		{"castilla-y-leon", ""}:                                "Castilla y Leon",
		{"spain", "España"}:                                    "España",
	}
	for in, want := range cases {
		if got := routingRegionDisplayName(in[0], in[1]); got != want {
			t.Errorf("routingRegionDisplayName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
