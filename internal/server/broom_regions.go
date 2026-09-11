package server

import (
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/clip"
	"github.com/paulmach/orb/geo"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/planar"
)

type routingSuggestion struct {
	RegionID   string `json:"regionId"`
	Name       string `json:"name"`
	Installed  bool   `json:"installed"`
	Active     bool   `json:"active"`
	CoversView bool   `json:"coversView"`
}

// Corners alone are insufficient: a concave boundary or a hole may cross the
// viewport between them. Reject any polygon boundary entering its interior.
func regionCoversView(polygons orb.MultiPolygon, view orb.Bound) bool {
	for _, corner := range view.ToRing() {
		if !planar.MultiPolygonContains(polygons, corner) {
			return false
		}
	}
	inner := orb.Bound{
		Min: orb.Point{math.Nextafter(view.Min[0], view.Max[0]), math.Nextafter(view.Min[1], view.Max[1])},
		Max: orb.Point{math.Nextafter(view.Max[0], view.Min[0]), math.Nextafter(view.Max[1], view.Min[1])},
	}
	for _, polygon := range polygons {
		for _, ring := range polygon {
			if len(clip.LineString(inner, orb.LineString(ring))) != 0 {
				return false
			}
		}
	}
	return true
}

func smallestRoutingRegion(index *geojson.FeatureCollection, views []orb.Bound) *routingSuggestion {
	var best *routingSuggestion
	var focused *routingSuggestion
	focusArea := math.Inf(1)
	bestParent := ""
	area := math.Inf(1)
	for _, feature := range index.Features {
		if feature == nil {
			continue
		}
		id, _ := feature.Properties["id"].(string)
		urls, _ := feature.Properties["urls"].(map[string]any)
		pbf, _ := urls["pbf"].(string)
		if id == "" || pbf == "" {
			continue
		}
		parent, _ := feature.Properties["parent"].(string)
		var polygons orb.MultiPolygon
		switch geometry := feature.Geometry.(type) {
		case orb.Polygon:
			polygons = orb.MultiPolygon{geometry}
		case orb.MultiPolygon:
			polygons = geometry
		default:
			continue
		}
		size := math.Abs(geo.Area(polygons))
		name, _ := feature.Properties["name"].(string)
		// A coastal view may extend beyond every country's offshore polygon.
		// Offer the local extract rather than escalating to an entire continent,
		// but explicitly report that it does not cover every viewport edge.
		if len(views) == 1 && parent != "" && planar.MultiPolygonContains(polygons, views[0].Center()) && size < focusArea && size >= math.Abs(geo.Area(views[0].ToPolygon()))/4 {
			focused = &routingSuggestion{RegionID: id, Name: valueOrDefault(name, id)}
			focusArea = size
		}
		covers := true
		for _, view := range views {
			if !regionCoversView(polygons, view) {
				covers = false
				break
			}
		}
		if !covers {
			continue
		}
		if size > area || (size == area && best != nil && id >= best.RegionID) {
			continue
		}
		best = &routingSuggestion{RegionID: id, Name: valueOrDefault(name, id), CoversView: true}
		bestParent = parent
		area = size
	}
	if focused != nil && (best == nil || bestParent == "") {
		return focused
	}
	return best
}

func (s *Server) handleBroomSuggest(w http.ResponseWriter, r *http.Request) {
	var request struct {
		BBox *bbox `json:"bbox"`
	}
	if err := decodeJSONBody(w, r, 4096, &request); err != nil || request.BBox == nil {
		writeError(w, http.StatusBadRequest, "viewport bbox is required")
		return
	}
	b := request.BBox
	if !(coordinate{Lat: b.South, Lon: b.West}).valid() || !(coordinate{Lat: b.North, Lon: b.East}).valid() || b.South >= b.North || b.West == b.East {
		writeError(w, http.StatusBadRequest, "invalid viewport bbox")
		return
	}
	views := []orb.Bound{{Min: orb.Point{b.West, b.South}, Max: orb.Point{b.East, b.North}}}
	if b.West > b.East {
		views = []orb.Bound{{Min: orb.Point{b.West, b.South}, Max: orb.Point{180, b.North}}, {Min: orb.Point{-180, b.South}, Max: orb.Point{b.East, b.North}}}
	}
	endpoint, _ := url.Parse(s.broom.indexURL) // Validated by Broom at startup.
	policy := newProviderPolicy("routing-index", "routing-index", endpoint, 24*time.Hour, 30*24*time.Hour, 90*24*time.Hour, true, 32<<20, []string{"application/json", "application/geo+json"}, newRateGroup(0), true)
	response, err := s.outbound.do(r.Context(), cachedRequest{policy: policy, method: http.MethodGet, url: endpoint.String(), params: "region-index", cacheable: true,
		validate: func(body []byte) error { _, err := geojson.UnmarshalFeatureCollection(body); return err },
	})
	if err != nil {
		writeOutboundError(w, err, policy.scope)
		return
	}
	index, err := geojson.UnmarshalFeatureCollection(response.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Invalid routing catalogue")
		return
	}
	suggestion := smallestRoutingRegion(index, views)
	if suggestion != nil {
		regions, _ := s.broom.manager.CachedRegions(r.Context())
		for _, region := range regions {
			if region.RegionID == suggestion.RegionID && region.Selected {
				suggestion.Installed = true
			}
		}
		s.broom.mu.RLock()
		suggestion.Active = s.broom.current != nil && s.broom.current.regionID == suggestion.RegionID
		s.broom.mu.RUnlock()
	}
	noStoreJSON(w, http.StatusOK, map[string]any{"region": suggestion})
}
