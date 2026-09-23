package server

import "errors"

var errTileQuotaPinned = errors.New("elevation tile cache quota is occupied by downloaded packs")

func (s *tileStore) setPackPins(owner string, paths []string) {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	if s.diskPins == nil {
		s.diskPins = make(map[string]map[string]struct{})
	}
	for path, owners := range s.diskPins {
		delete(owners, owner)
		if len(owners) == 0 {
			delete(s.diskPins, path)
		}
	}
	for _, path := range paths {
		if _, valid := tileKeyFromPath(path); !valid {
			continue
		}
		if s.diskPins[path] == nil {
			s.diskPins[path] = make(map[string]struct{})
		}
		s.diskPins[path][owner] = struct{}{}
	}
}

func (s *tileStore) canFitPack(keys []tileKey) bool {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	paths := make(map[string]struct{}, len(keys)+len(s.diskPins))
	for path := range s.diskPins {
		paths[path] = struct{}{}
	}
	for _, key := range keys {
		paths[key.path()] = struct{}{}
	}
	var required int64
	for path := range paths {
		if tile, ok := s.diskFiles[path]; ok {
			required += tile.size
		} else {
			required += 120 << 10
		}
	}
	quota := s.maxDiskBytes
	if s.useAvailableStorage {
		quota = availableStorageQuota(s.cacheDir, s.diskBytes)
	}
	return required <= quota
}

func (s *tileStore) finishPackRestore() error {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	err := s.enforceDiskQuotaLocked("")
	// A reduced configured quota must not destroy explicitly downloaded data.
	// New admissions will fail until packs are removed or the quota is raised.
	if errors.Is(err, errTileQuotaPinned) {
		return nil
	}
	return err
}

func (m *packManager) restoreElevationPins(changed map[string]bool) error {
	tiles := m.server.elevation.tiles
	if tiles == nil {
		return nil
	}
	for _, pack := range m.packs {
		tiles.setPackPins(pack.ID, pack.ElevationKeys)
		progress := pack.Resources[packResourceElevation]
		if pack.State != "complete" && !(progress.Total > 0 && progress.Done == progress.Total && progress.Failed == 0) {
			continue
		}
		if pack.Resources[packResourceElevation].Total > 0 && len(pack.ElevationKeys) == 0 {
			pack.State, pack.ErrorCode = "incomplete", "legacy_elevation_manifest"
			changed[pack.ID] = true
		}
		for _, path := range pack.ElevationKeys {
			key, valid := tileKeyFromPath(path)
			_, present := tiles.diskFileSize(key)
			if !valid || !present {
				pack.State, pack.ErrorCode = "incomplete", "missing_elevation_tiles"
				changed[pack.ID] = true
				break
			}
		}
	}
	return tiles.finishPackRestore()
}
