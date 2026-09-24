package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
	"github.com/paulmach/orb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackSurfacesKeepUncertainDistanceUnknown(t *testing.T) {
	annotation := &broom.TrackAnnotation{Spans: []broom.TrackSpan{
		{Distance: 100, Status: broom.TrackMatched, Match: &broom.TrackMatch{Surface: "asphalt"}},
		{Distance: 200, Status: broom.TrackMatched, Match: &broom.TrackMatch{Surface: "gravel"}},
		{Distance: 30, Status: broom.TrackMatched, Match: &broom.TrackMatch{}},
		{Distance: 40, Status: broom.TrackUnmatched},
		{Distance: 50, Status: broom.TrackAmbiguous, Candidates: []broom.TrackMatch{{Surface: "asphalt"}}},
	}}
	assert.Equal(t, []broomSurfaceDistance{
		{Surface: "", DistanceMeters: 120},
		{Surface: "asphalt", DistanceMeters: 100},
		{Surface: "gravel", DistanceMeters: 200},
	}, broomTrackSurfaces(annotation))
}

func TestBroomAnnotateTrackOffline(t *testing.T) {
	dir := t.TempDir()
	var inputs []broom.UnionInput
	for i, surface := range []string{"asphalt", "gravel"} {
		source := filepath.Join(dir, fmt.Sprintf("surface-%d.osm", i))
		xml := fmt.Sprintf(`<osm version="0.6">
<node id="%d" version="1" visible="true" lat="42" lon="%f"/><node id="%d" version="1" visible="true" lat="42" lon="%f"/>
<way id="%d" version="1" visible="true"><nd ref="%d"/><nd ref="%d"/><tag k="highway" v="track"/><tag k="surface" v="%s"/></way></osm>`,
			i+1, 1+float64(i)*0.001, i+2, 1+float64(i+1)*0.001, 11+i, i+1, i+2, surface)
		require.NoError(t, os.WriteFile(source, []byte(xml), 0600))
		inputs = append(inputs, broom.UnionInput{
			Path: source, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(xml))), Snapshot: time.Unix(1, 0),
			Coverage: orb.MultiPolygon{orb.Bound{Min: orb.Point{0.9, 41.9}, Max: orb.Point{1.1, 42.1}}.ToPolygon()},
		})
	}
	graph := filepath.Join(dir, "surfaces.broom")
	_, err := broom.BuildUnion(t.Context(), inputs, graph, broom.UnionOptions{})
	require.NoError(t, err)
	s, err := New(Config{GPXDir: t.TempDir(), RoutingCacheDir: t.TempDir(), RoutingGraph: graph, OfflineMode: "cache-only"})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	points := []broom.Point{{Lon: 1.0001, Lat: 42}, {Lon: 1.001, Lat: 42}, {Lon: 1.0019, Lat: 42}}
	body := `{"coordinates":[{"lon":1.0001,"lat":42},{"lon":1.001,"lat":42},{"lon":1.0019,"lat":42}]}`
	response := do(t, s, http.MethodPost, "/routing/broom/annotate", strings.NewReader(body))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result broomTrackResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	assert.Equal(t, 1, result.SchemaVersion)
	assert.InDelta(t, 149, result.DistanceMeters, 1)
	var total float64
	bySurface := make(map[string]float64)
	for _, item := range result.Surfaces {
		total += item.DistanceMeters
		bySurface[item.Surface] = item.DistanceMeters
	}
	assert.InDelta(t, result.DistanceMeters, total, 1e-6)
	assert.Greater(t, bySurface["asphalt"], 50.0)
	assert.Greater(t, bySurface["gravel"], 50.0)
	assert.NotContains(t, response.Body.String(), "coordinates", "annotation must not return replacement geometry")
	config := do(t, s, http.MethodGet, "/config", nil)
	assert.Contains(t, config.Body.String(), `"broomAnnotate":"/routing/broom/annotate"`)
	tooMuchWork := `{"coordinates":[` + strings.Repeat(`{"lat":42,"lon":1},{"lat":42,"lon":1.002},`, 3000) + `{"lat":42,"lon":1}]}`
	limited := do(t, s, http.MethodPost, "/routing/broom/annotate", strings.NewReader(tooMuchWork))
	assert.Equal(t, http.StatusUnprocessableEntity, limited.Code)
	assert.Contains(t, limited.Body.String(), "track_annotation_limit")

	unknown := do(t, s, http.MethodPost, "/routing/broom/annotate", strings.NewReader(`{"coordinates":[{"lat":43,"lon":1},{"lat":43,"lon":1.001}]}`))
	require.Equal(t, http.StatusOK, unknown.Code, unknown.Body.String())
	require.NoError(t, json.Unmarshal(unknown.Body.Bytes(), &result))
	require.Len(t, result.Surfaces, 1)
	assert.Empty(t, result.Surfaces[0].Surface)
	assert.InDelta(t, result.DistanceMeters, result.Surfaces[0].DistanceMeters, 1e-6)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.broom.annotateTrack(ctx, points)
	assert.ErrorIs(t, err, context.Canceled)
	for range cap(s.broom.slots) {
		s.broom.slots <- struct{}{}
	}
	busy := do(t, s, http.MethodPost, "/routing/broom/annotate", strings.NewReader(body))
	assert.Equal(t, http.StatusTooManyRequests, busy.Code)
	for len(s.broom.slots) > 0 {
		<-s.broom.slots
	}
}

func TestBroomAnnotateTrackInputLimits(t *testing.T) {
	s, err := New(Config{GPXDir: t.TempDir(), RoutingCacheDir: t.TempDir(), OfflineMode: "cache-only"})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	for _, body := range []string{
		`{}`, `{"coordinates":[{"lat":42,"lon":1}]}`,
		`{"coordinates":[{"lat":42},{"lat":42,"lon":1}]}`,
		`{"coordinates":[{"lat":91,"lon":1},{"lat":42,"lon":1}]}`,
		`{"coordinates":[` + strings.Repeat(`{"lat":42,"lon":1},`, maxBroomTrackPoints) + `{"lat":42,"lon":1}]}`,
		`{"coordinates":` + strings.Repeat(" ", maxBroomTrackBody) + `[]}`,
	} {
		response := do(t, s, http.MethodPost, "/routing/broom/annotate", strings.NewReader(body))
		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	}
	missing := do(t, s, http.MethodPost, "/routing/broom/annotate", strings.NewReader(`{"coordinates":[{"lat":42,"lon":1},{"lat":42,"lon":1.001}]}`))
	assert.Equal(t, http.StatusConflict, missing.Code)
	assert.Contains(t, missing.Body.String(), "routing_data_required")
}
