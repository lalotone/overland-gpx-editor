package server

import (
	"bytes"
	"io"
	"net/http"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
)

// Let Broom own catalogue decoding/selection while retaining the application's
// persistent cache and strict offline policy for catalogue requests.
type broomIndexTransport struct {
	base     http.RoundTripper
	outbound *outboundClient
	policy   *providerPolicy
}

func (t broomIndexTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet || r.URL.String() != t.policy.baseURL.String() {
		return t.base.RoundTrip(r)
	}
	result, err := t.outbound.do(r.Context(), cachedRequest{policy: t.policy, method: http.MethodGet, url: r.URL.String(), params: "region-index", cacheable: true,
		validate: func(body []byte) error { _, err := geojson.UnmarshalFeatureCollection(body); return err },
	})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: result.Status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(result.Body)), ContentLength: int64(len(result.Body)), Request: r}, nil
}

type routingSuggestion struct {
	RegionID         string  `json:"regionId"`
	Name             string  `json:"name"`
	Installed        bool    `json:"installed"`
	Active           bool    `json:"active"`
	CoversView       bool    `json:"coversView"`
	CoverageFraction float64 `json:"coverageFraction"`
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
	var coverage orb.Geometry = orb.Bound{Min: orb.Point{b.West, b.South}, Max: orb.Point{b.East, b.North}}
	if b.West > b.East {
		coverage = orb.MultiPolygon{
			(orb.Bound{Min: orb.Point{b.West, b.South}, Max: orb.Point{180, b.North}}).ToPolygon(),
			(orb.Bound{Min: orb.Point{-180, b.South}, Max: orb.Point{b.East, b.North}}).ToPolygon(),
		}
	}
	candidates, err := s.broom.manager.SuggestRegions(r.Context(), coverage)
	if err != nil {
		writeBroomError(w, err)
		return
	}
	var suggestion *routingSuggestion
	if len(candidates) > 0 {
		candidate := candidates[0]
		suggestion = &routingSuggestion{RegionID: candidate.Region.ID, Name: candidate.Region.Name, CoversView: candidate.CoversEntireArea, CoverageFraction: candidate.CoverageFraction}
		regions, _ := s.broom.manager.CachedRegions(r.Context())
		for _, region := range regions {
			if region.RegionID == suggestion.RegionID && region.Selected && !region.Slim && region.Elevation == broom.Auto {
				suggestion.Installed = true
			}
		}
		s.broom.mu.RLock()
		suggestion.Active = s.broom.current != nil && s.broom.current.regionID == suggestion.RegionID
		s.broom.mu.RUnlock()
	}
	noStoreJSON(w, http.StatusOK, map[string]any{"region": suggestion})
}

func (s *Server) handleBroomPlan(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RegionID string `json:"regionId"`
	}
	if err := decodeJSONBody(w, r, 4096, &request); err != nil || request.RegionID == "" {
		writeError(w, http.StatusBadRequest, "regionId is required")
		return
	}
	plan, err := s.broom.manager.PlanRegion(r.Context(), request.RegionID, broom.PlanOptions{Setup: broom.SetupOptions{Jobs: s.broom.jobs, Elevation: broom.Auto}})
	if err != nil {
		writeBroomError(w, err)
		return
	}
	noStoreJSON(w, http.StatusOK, map[string]any{"pbfBytes": plan.PBFBytes, "estimatedBytes": plan.EstimatedBytes, "tilesKnown": plan.DEMTilesKnown, "tilesTotal": len(plan.DEMTiles), "tilesCached": len(plan.CachedDEMTiles), "tilesMissing": len(plan.MissingDEMTiles), "installed": plan.Installed})
}
