package server

import (
	"context"
	"net/http"
	"slices"
	"strings"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
)

const (
	maxBroomTrackBody   = 4 << 20
	maxBroomTrackPoints = 50001 // Broom also bounds internally sampled intervals.
)

type broomSurfaceDistance struct {
	Surface        string  `json:"surface"`
	DistanceMeters float64 `json:"distanceMeters"`
}

type broomTrackResponse struct {
	SchemaVersion  int                    `json:"schemaVersion"`
	RegionID       string                 `json:"regionId,omitempty"`
	GenerationID   string                 `json:"generationId,omitempty"`
	DistanceMeters float64                `json:"distanceMeters"`
	Surfaces       []broomSurfaceDistance `json:"surfaces"`
}

// Keep original-track distance, including uncertain intervals, in the total.
// Candidates on ambiguous spans are not evidence for any surface classification.
func broomTrackSurfaces(annotation *broom.TrackAnnotation) []broomSurfaceDistance {
	distances := make(map[string]float64)
	for _, span := range annotation.Spans {
		surface := ""
		if span.Status == broom.TrackMatched && span.Match != nil {
			surface = span.Match.Surface
		}
		distances[surface] += span.Distance
	}
	result := make([]broomSurfaceDistance, 0, len(distances))
	for surface, distance := range distances {
		result = append(result, broomSurfaceDistance{Surface: surface, DistanceMeters: distance})
	}
	slices.SortFunc(result, func(a, b broomSurfaceDistance) int { return strings.Compare(a.Surface, b.Surface) })
	return result
}

func (s *broomRoutingService) annotateTrack(ctx context.Context, points []broom.Point) (broomTrackResponse, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return broomTrackResponse{}, &routingBusyError{}
	}
	queryCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	stopRootCancel := context.AfterFunc(s.ctx, cancel)
	defer stopRootCancel()
	s.mu.RLock()
	dataset := s.current
	if dataset == nil || !dataset.acquire() {
		s.mu.RUnlock()
		return broomTrackResponse{}, errRoutingNotReady
	}
	s.mu.RUnlock()
	defer dataset.release()
	// No profiles, metric writes, source downloads, or replacement geometry.
	annotation, err := dataset.router.AnnotateTrack(queryCtx, points, broom.TrackAnnotationOptions{})
	if err != nil {
		return broomTrackResponse{}, err
	}
	return broomTrackResponse{
		SchemaVersion: 1, RegionID: dataset.regionID, GenerationID: dataset.generationID,
		DistanceMeters: annotation.Distance, Surfaces: broomTrackSurfaces(annotation),
	}, nil
}

func (s *Server) handleBroomAnnotate(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Coordinates []requiredCoordinate `json:"coordinates"`
	}
	if err := decodeJSONBody(w, r, maxBroomTrackBody, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(request.Coordinates) < 2 || len(request.Coordinates) > maxBroomTrackPoints {
		writeError(w, http.StatusBadRequest, "track must contain 2..50001 coordinates; split larger tracks")
		return
	}
	points := make([]broom.Point, len(request.Coordinates))
	for i, raw := range request.Coordinates {
		point, err := raw.coordinate()
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		points[i] = broom.Point{Lat: point.Lat, Lon: point.Lon}
	}
	response, err := s.broom.annotateTrack(r.Context(), points)
	if err != nil {
		writeBroomError(w, err)
		return
	}
	noStoreJSON(w, http.StatusOK, response)
}
