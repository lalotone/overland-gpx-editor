package server

import (
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

func TestMotorcycleAccessPermit(t *testing.T) {
	for _, source := range []string{broomProfileSource, broomEnduroSource} {
		profile, _, err := broom.ParseProfile(strings.NewReader(source), "test-motorcycle")
		require.NoError(t, err)
		permit, err := profile.With(map[string]float64{"overland_access_permit": 1})
		require.NoError(t, err)
		assert.NotEqual(t, profile.Hash(), permit.Hash())
		for _, tt := range []struct {
			name                          string
			tags                          map[string]string
			defaultAllowed, permitAllowed bool
		}{
			{"track", map[string]string{"highway": "track"}, true, true},
			{"untagged path", map[string]string{"highway": "path"}, false, true},
			{"general permission on path", map[string]string{"highway": "path", "access": "yes"}, false, true},
			{"motor permission", map[string]string{"highway": "path", "motorcycle": "designated"}, true, true},
			{"specific restriction", map[string]string{"highway": "path", "motor_vehicle": "yes", "motorcycle": "no"}, false, true},
			{"private", map[string]string{"highway": "track", "access": "private"}, false, true},
			{"motor restriction", map[string]string{"highway": "track", "motor_vehicle": "no"}, false, true},
			{"not a road", map[string]string{}, false, false},
			{"impassable", map[string]string{"highway": "track", "smoothness": "impassable"}, false, false},
			{"wrong way", map[string]string{"highway": "track", "oneway": "-1"}, false, false},
		} {
			t.Run(tt.name, func(t *testing.T) {
				for _, policy := range []struct {
					profile *broom.Profile
					allowed bool
				}{{profile, tt.defaultAllowed}, {permit, tt.permitAllowed}} {
					result, err := policy.profile.EvalWay(tt.tags, false)
					require.NoError(t, err)
					assert.Equal(t, policy.allowed, result.CostFactor < 10000, "tags %v", tt.tags)
				}
			})
		}
		for _, barrier := range []string{"gate", "lift_gate", "swing_gate", "chain", "bollard", "block", "fence"} {
			tags := map[string]string{"barrier": barrier, "access": "private"}
			closed, err := profile.EvalNode(tags, broom.WayResult{}, false)
			require.NoError(t, err)
			open, err := permit.EvalNode(tags, broom.WayResult{}, false)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, closed.InitialCost, float32(1000000))
			assert.Equal(t, barrier == "gate" || barrier == "lift_gate" || barrier == "swing_gate" || barrier == "chain", open.InitialCost < 1000000, barrier)
		}
	}
}

func TestEnduroDefaultMatchesBroom(t *testing.T) {
	upstream, ok := broom.BuiltinProfile("enduro")
	require.True(t, ok)
	ours, _, err := broom.ParseProfile(strings.NewReader(broomEnduroSource), "overland-enduro")
	require.NoError(t, err)
	for _, highway := range []string{"track", "path", "bridleway", "footway", "cycleway", "steps", "residential", "motorway", "service", ""} {
		for _, surface := range []string{"gravel", "asphalt", "mud", ""} {
			for _, access := range []string{"", "yes", "private", "destination", "forestry"} {
				for _, permission := range []string{"", "yes", "no"} {
					tags := map[string]string{"highway": highway, "surface": surface, "access": access, "motorcycle": permission}
					for _, reverse := range []bool{false, true} {
						want, err := upstream.EvalWay(tags, reverse)
						require.NoError(t, err)
						got, err := ours.EvalWay(tags, reverse)
						require.NoError(t, err)
						assert.EqualExportedValues(t, want, got, "tags %v", tags)
					}
				}
			}
		}
	}
}

// A restricted bridge and an access gate split two large motor components.
// Endpoints are far enough away that snapping cannot jump over the closure.
func TestPermitRoutesAcrossRestrictedPass(t *testing.T) {
	var xml strings.Builder
	xml.WriteString(`<osm version="0.6">`)
	for i := range 120 {
		fmt.Fprintf(&xml, `<node id="%d" version="1" visible="true" lat="42" lon="%f">`, i+1, 1+float64(i)*0.001)
		if i == 60 {
			xml.WriteString(`<tag k="barrier" v="gate"/><tag k="access" v="private"/>`)
		}
		xml.WriteString(`</node>`)
	}
	var right strings.Builder
	right.WriteString(xml.String())
	for i := range 119 {
		dst := &xml
		if i >= 60 {
			dst = &right
		}
		fmt.Fprintf(dst, `<way id="%d" version="1" visible="true"><nd ref="%d"/><nd ref="%d"/><tag k="highway" v="track"/>`, 1000+i, i+1, i+2)
		if i == 60 {
			dst.WriteString(`<tag k="motorcycle" v="private"/>`)
		}
		dst.WriteString(`</way>`)
	}
	xml.WriteString(`</osm>`)
	right.WriteString(`</osm>`)
	dir := t.TempDir()
	var inputs []broom.UnionInput
	for i, contents := range []string{xml.String(), right.String()} {
		source := filepath.Join(dir, fmt.Sprintf("pass-%d.osm", i))
		require.NoError(t, os.WriteFile(source, []byte(contents), 0600))
		inputs = append(inputs, broom.UnionInput{
			Path: source, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(contents))), Snapshot: time.Unix(1, 0),
			Coverage: orb.MultiPolygon{orb.Bound{Min: orb.Point{0.9, 41.9}, Max: orb.Point{1.2, 42.1}}.ToPolygon()},
		})
	}
	graph := filepath.Join(dir, "pass.broom")
	_, err := broom.BuildUnion(t.Context(), inputs, graph, broom.UnionOptions{})
	require.NoError(t, err)
	s, err := New(Config{GPXDir: t.TempDir(), RoutingCacheDir: t.TempDir(), RoutingGraph: graph})
	require.NoError(t, err)
	cleanupTestServer(t, s)
	for _, profile := range []string{"road", "mixed", "trail", "enduro"} {
		t.Run(profile, func(t *testing.T) {
			for _, permit := range []bool{false, true, false} {
				body := fmt.Sprintf(`{"profile":%q,"accessPermit":%t,"waypoints":[{"lat":42,"lon":1.001},{"lat":42,"lon":1.118}]}`, profile, permit)
				result := do(t, s, http.MethodPost, "/routing/broom/route", strings.NewReader(body))
				if !permit {
					assert.Equal(t, http.StatusUnprocessableEntity, result.Code, result.Body.String())
					continue
				}
				require.Equal(t, http.StatusOK, result.Code, result.Body.String())
				var route broomRouteResponse
				require.NoError(t, json.Unmarshal(result.Body.Bytes(), &route))
				assert.True(t, route.AccessPermit)
				assert.Greater(t, route.DistanceMeters, 9000.0)
			}
		})
	}
	for _, profile := range []string{"custom", "mixed-permit"} {
		body := fmt.Sprintf(`{"profile":%q,"accessPermit":true,"waypoints":[{"lat":42,"lon":1.001},{"lat":42,"lon":1.118}]}`, profile)
		result := do(t, s, http.MethodPost, "/routing/broom/route", strings.NewReader(body))
		assert.Equal(t, http.StatusBadRequest, result.Code)
	}
}
