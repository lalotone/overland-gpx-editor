package server

import (
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// peerIsLoopback reports whether the request came from this machine. Behind a
// reverse proxy every request arrives from the proxy, so a loopback peer
// address proves nothing: implicit trust is withdrawn entirely and callers
// must present an admin token or an explicitly trusted origin instead.
func (s *Server) peerIsLoopback(r *http.Request) bool {
	return !s.behindProxy && remoteIsLoopback(r.RemoteAddr)
}

func remoteIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) validAdminToken(r *http.Request) bool {
	if s.adminToken == "" {
		return false
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	token, ok := strings.CutPrefix(value, "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) == 1
}

func (s *Server) isTrustedResourceOrigin(origin string, r *http.Request) bool {
	normalized, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	if normalized == s.trustedUIOrigin {
		return true
	}
	_, allowed := s.allowedOrigins[normalized]
	return allowed || (s.peerIsLoopback(r) && loopbackOrigin(normalized))
}

func (s *Server) isAllowedResourceHost(host string) bool {
	for origin := range s.allowedOrigins {
		u, _ := url.Parse(origin)
		if strings.EqualFold(u.Host, host) {
			return true
		}
	}
	return false
}

func (s *Server) isTrustedManagementOrigin(origin string, r *http.Request) bool {
	normalized, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	if s.trustedUIOrigin != "" {
		return normalized == s.trustedUIOrigin
	}
	return s.peerIsLoopback(r) && loopbackOrigin(normalized)
}

func (s *Server) authorizedOfflineControl(r *http.Request, requireHeader bool) bool {
	if s.validAdminToken(r) {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return s.peerIsLoopback(r)
	}
	if requireHeader && r.Header.Get("X-GPX-Editor") == "" {
		return false
	}
	return s.isTrustedManagementOrigin(origin, r)
}

func (s *Server) requireOfflineControl(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizedOfflineControl(r, true) {
			writeError(w, http.StatusForbidden, "Offline management request refused")
			return
		}
		next(w, r)
	}
}

func (s *Server) requireOfflineRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizedOfflineControl(r, false) {
			writeError(w, http.StatusForbidden, "Offline management request refused")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleOfflineOptions(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	requested := strings.ToLower(r.Header.Get("Access-Control-Request-Headers"))
	if origin == "" || !s.isTrustedManagementOrigin(origin, r) || !strings.Contains(requested, "x-gpx-editor") {
		writeError(w, http.StatusForbidden, "Offline management preflight refused")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func noStoreJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, payload)
}

func (s *Server) handleOfflineStatus(w http.ResponseWriter, _ *http.Request) {
	legacyBytes, legacyMaxBytes, legacyEntries := int64(0), int64(0), 0
	if s.elevation.tiles != nil {
		legacyBytes, legacyEntries = s.elevation.tiles.diskStats()
		legacyMaxBytes = s.elevation.tiles.diskQuota()
	}
	type jobAggregate struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Done     int    `json:"done"`
		Total    int    `json:"total"`
		Failures int    `json:"failed"`
		Bytes    int64  `json:"bytes"`
		Active   int    `json:"active"`
	}
	active := jobAggregate{ID: "active", State: "queued"}
	if s.packs != nil {
		for _, job := range s.packs.summaries() {
			if job.State == "queued" || job.State == "running" {
				active.Active++
				active.Done += job.Done
				active.Total += job.Total
				active.Failures += job.Failures
				active.Bytes += job.Bytes
				if job.State == "running" {
					active.State = "running"
				}
			}
		}
	}
	jobs := []jobAggregate{}
	if active.Active > 0 {
		jobs = append(jobs, active)
	}
	cache := s.cache.stats()
	noStoreJSON(w, http.StatusOK, struct {
		Enabled    bool                      `json:"enabled"`
		Mode       offlineMode               `json:"mode"`
		Writable   bool                      `json:"writable"`
		Bytes      int64                     `json:"bytes"`
		MaxBytes   int64                     `json:"maxBytes"`
		Entries    int                       `json:"entries"`
		MaxEntries int                       `json:"maxEntries"`
		Scopes     map[string]cacheScopeStat `json:"scopes"`
		Cache      cacheStats                `json:"cache"`
		Elevation  struct {
			Bytes    int64 `json:"bytes"`
			MaxBytes int64 `json:"maxBytes"`
			Entries  int   `json:"entries"`
		} `json:"elevationTiles"`
		Providers  map[string]providerHealth `json:"providers"`
		Jobs       []jobAggregate            `json:"jobs"`
		ActiveJobs int                       `json:"activeJobs"`
	}{
		Enabled: true, Mode: s.modes.mode(), Writable: cache.Writable, Bytes: cache.Bytes,
		MaxBytes: cache.Quota, Entries: cache.Entries, MaxEntries: s.cache.maxEntries,
		Scopes: cache.Scopes, Cache: cache,
		Elevation: struct {
			Bytes    int64 `json:"bytes"`
			MaxBytes int64 `json:"maxBytes"`
			Entries  int   `json:"entries"`
		}{legacyBytes, legacyMaxBytes, legacyEntries},
		Providers: s.outbound.healthSnapshot(), Jobs: jobs, ActiveJobs: active.Active,
	})
}

func (s *Server) handleOfflineMode(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Mode offlineMode `json:"mode"`
	}
	if err := decodeJSONBody(w, r, 4<<10, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	changed, err := s.modes.set(request.Mode)
	if err != nil {
		status := http.StatusBadRequest
		if request.Mode == modeAuto && !s.modes.canToggle() {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	if changed {
		slog.Info("Offline mode changed", "mode", request.Mode)
		if request.Mode == modeAuto && s.broom != nil {
			s.broom.resumeUpgrade()
		}
		if request.Mode == modeAuto && s.openFreeMap != nil && !s.openFreeMap.isActive() {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.openFreeMap.activate(s.ctx)
			}()
		}
	}
	noStoreJSON(w, http.StatusOK, struct {
		Mode    offlineMode `json:"mode"`
		Changed bool        `json:"changed"`
	}{Mode: s.modes.mode(), Changed: changed})
}

func (s *Server) handleClearCache(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope != "" && !validScope(scope) {
		writeError(w, http.StatusBadRequest, "invalid cache scope")
		return
	}
	removed, err := s.cache.clear(scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Cache deletion failed")
		return
	}
	noStoreJSON(w, http.StatusOK, map[string]int{"removed": removed})
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	type openFreeMapCapability struct {
		Style     string `json:"style"`
		AllowBulk bool   `json:"allowBulk"`
	}
	type configResponse struct {
		NominatimURL string `json:"nominatimUrl"`
		Offline      struct {
			Enabled     bool        `json:"enabled"`
			Mode        offlineMode `json:"mode"`
			Status      string      `json:"status"`
			Packs       string      `json:"packs"`
			ModeControl string      `json:"modeControl,omitempty"`
			Routing     string      `json:"routing,omitempty"`
		} `json:"offline"`
		Services map[string]string `json:"services"`
		Maps     struct {
			Raster      map[string]string      `json:"raster"`
			OpenFreeMap *openFreeMapCapability `json:"openfreemap,omitempty"`
		} `json:"maps"`
	}
	var response configResponse
	response.NominatimURL = s.nominatimURL
	response.Offline.Enabled = true
	response.Offline.Mode = s.modes.mode()
	response.Offline.Status = "/offline/status"
	response.Offline.Packs = "/offline/packs"
	if s.modes.canToggle() {
		response.Offline.ModeControl = "/offline/mode"
	}
	response.Services = map[string]string{"fuel": "/fuel", "places": "/places/search", "pois": "/pois/search"}
	if s.broom != nil {
		response.Services["broomRoute"] = "/routing/broom/route"
		response.Services["broomAnnotate"] = "/routing/broom/annotate"
		response.Offline.Routing = "/offline/routing"
	}
	response.Maps.Raster = map[string]string{
		"osm":      "/map/raster/osm/{z}/{x}/{y}.png",
		"opentopo": "/map/raster/opentopo/{z}/{x}/{y}.png",
		"cyclosm":  "/map/raster/cyclosm/{z}/{x}/{y}.png",
	}
	if s.openFreeMap != nil {
		response.Maps.OpenFreeMap = &openFreeMapCapability{Style: "/map/openfreemap/style.json", AllowBulk: s.openFreeMap.allowBulk}
	}
	writeJSON(w, http.StatusOK, response)
}
