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

type broomTrackPosition struct {
	Index    int     `json:"index"`
	Fraction float64 `json:"fraction"`
}

// broomTrackSpan places one matched or uncertain stretch on the original
// track, so the client can colour and flag the GPX it already has. Road holds
// the lookup-normalized OSM tags only; it is not an access verdict.
type broomTrackSpan struct {
	Start          broomTrackPosition    `json:"start"`
	End            broomTrackPosition    `json:"end"`
	DistanceMeters float64               `json:"distanceMeters"`
	Matched        bool                  `json:"matched"`
	Surface        string                `json:"surface,omitempty"`
	Road           *broom.RoadAttributes `json:"road,omitempty"`
}

type broomTrackResponse struct {
	SchemaVersion  int                    `json:"schemaVersion"`
	RegionID       string                 `json:"regionId,omitempty"`
	GenerationID   string                 `json:"generationId,omitempty"`
	DistanceMeters float64                `json:"distanceMeters"`
	Surfaces       []broomSurfaceDistance `json:"surfaces"`
	Spans          []broomTrackSpan       `json:"spans"`
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

// broomTrackSpans keeps the same matched/uncertain split as the surface
// totals and merges neighbours that carry identical tags, which is most of a
// track: one way is usually many sampling intervals long.
func broomTrackSpans(annotation *broom.TrackAnnotation) []broomTrackSpan {
	result := make([]broomTrackSpan, 0, len(annotation.Spans))
	for _, span := range annotation.Spans {
		next := broomTrackSpan{
			Start:          broomTrackPosition{Index: span.Start.Index, Fraction: span.Start.Fraction},
			End:            broomTrackPosition{Index: span.End.Index, Fraction: span.End.Fraction},
			DistanceMeters: span.Distance,
		}
		if span.Status == broom.TrackMatched && span.Match != nil {
			road := span.Match.Road
			// Direction relative to the OSM way says nothing about access and
			// would stop otherwise identical neighbours merging.
			road.Reversed = false
			next.Matched, next.Surface, next.Road = true, span.Match.Surface, &road
		}
		if n := len(result); n > 0 && sameTrackTags(result[n-1], next) && result[n-1].End == next.Start {
			result[n-1].End = next.End
			result[n-1].DistanceMeters += next.DistanceMeters
			continue
		}
		result = append(result, next)
	}
	return result
}

func sameTrackTags(a, b broomTrackSpan) bool {
	if a.Matched != b.Matched || a.Surface != b.Surface || (a.Road == nil) != (b.Road == nil) {
		return false
	}
	return a.Road == nil || *a.Road == *b.Road
}

func (s *broomRoutingService) annotateTrack(ctx context.Context, points []broom.Point) (broomTrackResponse, error) {
	owner, _ := OwnerFrom(ctx)
	release, ok := s.acquireSlot(owner)
	if !ok {
		return broomTrackResponse{}, &routingBusyError{}
	}
	defer release()
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
		Spans: broomTrackSpans(annotation),
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
