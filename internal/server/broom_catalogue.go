package server

import (
	"net/http"
	"slices"
	"strings"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
	"github.com/paulmach/orb"
)

type routingRegionEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Parent    string `json:"parent,omitempty"`
	Kind      string `json:"kind"`
	BBox      *bbox  `json:"bbox,omitempty"`
	Installed bool   `json:"installed"`
	Active    bool   `json:"active"`
}

func (s *Server) handleBroomRegions(w http.ResponseWriter, r *http.Request) {
	regions, catalogErr := s.broom.manager.ListRegions(r.Context())
	cached, err := s.broom.manager.CachedRegions(r.Context())
	if err != nil {
		writeBroomError(w, err)
		return
	}
	installed := make(map[string]broom.CachedRegion)
	for _, region := range cached {
		if region.Selected && !region.Slim && region.Elevation == broom.Auto {
			installed[region.RegionID] = region
		}
	}
	// Cached generations remain browsable without a cached provider catalogue.
	if catalogErr != nil {
		if len(installed) == 0 {
			writeBroomError(w, catalogErr)
			return
		}
		regions = nil
		for _, region := range installed {
			regions = append(regions, broom.Region{ID: region.RegionID, Name: region.Name, Coverage: region.Coverage})
		}
	}
	s.broom.mu.RLock()
	active := ""
	if s.broom.current != nil {
		active = s.broom.current.regionID
	}
	s.broom.mu.RUnlock()
	entries := make([]routingRegionEntry, 0, len(regions))
	for _, region := range regions {
		_, downloaded := installed[region.ID]
		kind := "region"
		if routingContinent(region.ID) {
			kind = "continent"
		} else if catalogErr == nil && (routingContinent(region.Parent) || region.Parent == "") {
			kind = "country"
		}
		entries = append(entries, routingRegionEntry{ID: region.ID, Name: region.Name, Parent: region.Parent, Kind: kind, BBox: routingRegionBounds(region.Coverage), Installed: downloaded, Active: region.ID == active})
	}
	slices.SortFunc(entries, func(a, b routingRegionEntry) int { return strings.Compare(a.Name, b.Name) })
	noStoreJSON(w, http.StatusOK, map[string]any{"regions": entries, "cachedOnly": catalogErr != nil})
}

func routingContinent(id string) bool {
	switch id {
	case "africa", "antarctica", "asia", "australia-oceania", "central-america", "europe", "north-america", "south-america":
		return true
	default:
		return false
	}
}

func routingRegionBounds(coverage orb.MultiPolygon) *bbox {
	if len(coverage) == 0 {
		return nil
	}
	b := coverage.Bound()
	if !(coordinate{Lat: b.Min[1], Lon: b.Min[0]}).valid() || !(coordinate{Lat: b.Max[1], Lon: b.Max[0]}).valid() || b.Min[0] >= b.Max[0] || b.Min[1] >= b.Max[1] {
		return nil
	}
	return &bbox{South: b.Min[1], West: b.Min[0], North: b.Max[1], East: b.Max[0]}
}
