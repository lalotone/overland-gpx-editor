package server

import (
	"net/http"
	"testing"
)

func TestCachePermittedVary(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"no vary", http.Header{}, true},
		{"encoding", http.Header{"Vary": {"Accept-Encoding"}}, true},
		// oms answers every request with Access-Control-Allow-Origin and
		// Vary: Origin; outbound requests send no Origin, so the body is
		// the same for every fetch and may be kept.
		{"origin", http.Header{"Vary": {"Origin"}}, true},
		{"origin and encoding", http.Header{"Vary": {"Origin, Accept-Encoding"}}, true},
		{"cookie", http.Header{"Vary": {"Cookie"}}, false},
		{"star", http.Header{"Vary": {"*"}}, false},
		{"no-store", http.Header{"Cache-Control": {"no-store"}}, false},
		{"private", http.Header{"Cache-Control": {"private"}}, false},
	}
	for _, tc := range cases {
		if got := cachePermitted(tc.header, false); got != tc.want {
			t.Errorf("%s: cachePermitted = %v, want %v", tc.name, got, tc.want)
		}
	}
}
