package server

import (
	"context"
	"errors"
	"fmt"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
)

// Keep usable old graphs available during migration, including in cache-only
// mode. Direct opening is supported by Broom across pipeline versions; a graph
// format that can no longer be read still fails normally. Corruption never
// triggers this fallback or an implicit source download.
func (s *broomRoutingService) openInstalledRegion(ctx context.Context, region string) (*broomDataset, error) {
	ready, err := s.manager.OpenRegion(ctx, region, broom.OpenRegionOptions{})
	var dataset *broomDataset
	if errors.Is(err, broom.ErrIncompatibleGeneration) {
		regions, listErr := s.manager.CachedRegions(ctx)
		if listErr != nil {
			return nil, listErr
		}
		for _, cached := range regions {
			if cached.RegionID != region || !cached.Selected || cached.Slim || cached.Elevation != broom.Auto {
				continue
			}
			router, openErr := s.manager.Open(cached.GraphPath)
			if openErr != nil {
				return nil, fmt.Errorf("open previous routing graph: %w", openErr)
			}
			dataset = newBroomDataset(router, cached.RegionID, cached.GenerationID, cached.Name)
			dataset.needsUpgrade = true
			break
		}
	}
	if dataset == nil {
		if err != nil {
			return nil, err
		}
		dataset = datasetFromSetup(ready)
	}
	if err := s.warmRouter(ctx, dataset.router); err != nil {
		_ = dataset.close()
		return nil, fmt.Errorf("prepare riding profiles: %w", err)
	}
	return dataset, nil
}

// Startup and an explicit return to online mode resume a deferred migration.
// Failed/cancelled jobs remain visible; status polling never starts work.
func (s *broomRoutingService) resumeUpgrade() {
	if s.modes.mode() == modeCacheOnly {
		return
	}
	s.mu.RLock()
	region := ""
	if s.current != nil && s.current.needsUpgrade {
		region = s.current.regionID
	}
	s.mu.RUnlock()
	if region == "" {
		return
	}
	if err := s.startPreparation(region, false); err != nil && !errors.Is(err, errRoutingPreparationRunning) {
		s.mu.Lock()
		s.lastErr = err.Error()
		s.mu.Unlock()
	}
}
