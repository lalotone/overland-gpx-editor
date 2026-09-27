package server

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// routeClass says whose data a route touches. Every route is registered with
// one, and a test walks the router to make sure none is missing, so a new
// endpoint cannot quietly serve one owner's data to another.
type routeClass int

const (
	_ routeClass = iota
	// publicRoute serves shared data: map tiles, routing, the fuel snapshot.
	// It needs no owner, though it may read one when present.
	publicRoute
	// ownerRoute serves the caller's own data and is refused without an
	// owner. Under sign-in that is every user; the check is the wrapper's.
	ownerRoute
	// operatorRoute changes server-wide state. Under sign-in it additionally
	// needs the operator mark or the admin token; see requireOperator.
	operatorRoute
)

// routeTable registers routes with their class. Handlers are wrapped so an
// owner-scoped route never runs without an owner and an operator route never
// runs for an ordinary user.
type routeTable struct {
	server *Server
	router chi.Router
}

func (t routeTable) handle(method, pattern string, class routeClass, handler http.HandlerFunc) {
	switch class {
	case ownerRoute:
		handler = t.server.requireOwnerRoute(handler)
	case operatorRoute:
		handler = t.server.requireOwnerRoute(t.server.requireOperator(handler))
	}
	t.server.routeClasses[method+" "+pattern] = class
	t.router.Method(method, pattern, handler)
}

func (t routeTable) get(pattern string, class routeClass, handler http.HandlerFunc) {
	t.handle(http.MethodGet, pattern, class, handler)
}

func (t routeTable) post(pattern string, class routeClass, handler http.HandlerFunc) {
	t.handle(http.MethodPost, pattern, class, handler)
}

func (t routeTable) put(pattern string, class routeClass, handler http.HandlerFunc) {
	t.handle(http.MethodPut, pattern, class, handler)
}

func (t routeTable) delete(pattern string, class routeClass, handler http.HandlerFunc) {
	t.handle(http.MethodDelete, pattern, class, handler)
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Use(s.attachOwner)
	r.Use(skipPath(mcpBrowserEventsPath, middleware.ThrottleBacklog(maxInFlightRequests, maxQueuedRequests, 5*time.Second)))
	r.Use(middleware.Compress(5))
	r.Use(middleware.GetHead)
	r.Use(s.protectBrowserWrites)
	if len(s.allowedOrigins) > 0 {
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins:     mapKeys(s.allowedOrigins),
			AllowedMethods:     []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
			AllowedHeaders:     []string{"Accept", "Content-Type", "Authorization", "X-GPX-Editor"},
			ExposedHeaders:     []string{"X-GPX-Cache", "X-GPX-Cached-At", "Age"},
			AllowCredentials:   false,
			MaxAge:             300,
			OptionsPassthrough: true,
		}))
	}
	s.routeClasses = make(map[string]routeClass)
	t := routeTable{server: s, router: r}

	t.get("/healthz", publicRoute, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	t.get("/files", ownerRoute, s.handleListFiles)
	t.get("/gpx/{filename}", ownerRoute, s.handleGetFile)
	t.put("/gpx/{filename}", ownerRoute, s.handleSaveFile)
	t.post("/gpx/{filename}", ownerRoute, s.handleSaveFile)
	t.delete("/gpx/{filename}", ownerRoute, s.handleDeleteFile)
	t.post("/upload", ownerRoute, s.handleUpload)
	// Elevation lookups name the exact points of the caller's track, so the
	// API-backed cache is owner-scoped; terrain tiles are shared.
	t.get("/elevation", ownerRoute, s.protectOutboundResource(s.handleElevation))
	t.post("/elevation/batch", ownerRoute, s.protectOutboundResource(s.handleElevationBatch))
	t.post("/elevation/prefetch", ownerRoute, s.protectOutboundResource(s.handlePrefetch))
	t.get("/elevation/prefetch", ownerRoute, s.handlePrefetchStatus)
	t.get("/fuel", publicRoute, s.protectOutboundResource(s.handleFuel))
	t.get("/places/search", ownerRoute, s.protectOutboundResource(s.handlePlaceSearch))
	t.post("/pois/search", publicRoute, s.protectOutboundResource(s.handlePOISearch))
	if s.broom != nil {
		t.post("/routing/broom/route", ownerRoute, s.protectOutboundResource(s.handleBroomRoute))
		t.post("/routing/broom/annotate", ownerRoute, s.protectOutboundResource(s.handleBroomAnnotate))
	}
	t.get("/map/raster/{layer}/{z}/{x}/{y}", publicRoute, s.protectOutboundResource(s.handleRasterMap))
	openFreeMap := func(serve func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return s.protectOutboundResource(func(w http.ResponseWriter, r *http.Request) {
			if s.openFreeMap == nil {
				writeError(w, http.StatusNotFound, "OpenFreeMap-compatible source is not configured")
				return
			}
			serve(w, r)
		})
	}
	t.get("/map/openfreemap/style.json", publicRoute, openFreeMap(func(w http.ResponseWriter, r *http.Request) { s.openFreeMap.handleStyle(w, r) }))
	t.get("/map/openfreemap/source/{source}", publicRoute, openFreeMap(func(w http.ResponseWriter, r *http.Request) { s.openFreeMap.handleSource(w, r) }))
	t.get("/map/openfreemap/source.json", publicRoute, openFreeMap(func(w http.ResponseWriter, r *http.Request) { s.openFreeMap.handleSource(w, r) }))
	t.get("/map/openfreemap/tiles/{source}/{z}/{x}/{y}", publicRoute, openFreeMap(func(w http.ResponseWriter, r *http.Request) { s.openFreeMap.handleTile(w, r, false) }))
	t.get("/map/openfreemap/tiles/{z}/{x}/{y}", publicRoute, openFreeMap(func(w http.ResponseWriter, r *http.Request) { s.openFreeMap.handleTile(w, r, false) }))
	t.get("/map/openfreemap/raster/{source}/{z}/{x}/{y}", publicRoute, openFreeMap(func(w http.ResponseWriter, r *http.Request) { s.openFreeMap.handleTile(w, r, true) }))
	t.get("/map/openfreemap/glyphs/{fontstack}/{range}", publicRoute, openFreeMap(func(w http.ResponseWriter, r *http.Request) { s.openFreeMap.handleGlyph(w, r) }))
	t.get("/map/openfreemap/{variant:sprite(?:@2x)?\\.(?:json|png)}", publicRoute, openFreeMap(func(w http.ResponseWriter, r *http.Request) { s.openFreeMap.handleSprite(w, r) }))
	// Status reports the caller's own jobs alongside shared cache totals.
	t.get("/offline/status", ownerRoute, s.handleOfflineStatus)
	t.put("/offline/mode", operatorRoute, s.requireOfflineControl(s.handleOfflineMode))
	t.get("/offline/packs", ownerRoute, s.requireOfflineRead(s.handleListPacks))
	t.post("/offline/packs/estimate", ownerRoute, s.requireOfflineControl(s.handleEstimatePack))
	t.post("/offline/packs", ownerRoute, s.requireOfflineControl(s.handleCreatePack))
	t.get("/offline/packs/{id}", ownerRoute, s.requireOfflineRead(s.handleGetPack))
	t.post("/offline/packs/{id}/cancel", ownerRoute, s.requireOfflineControl(s.handleCancelPack))
	t.delete("/offline/packs/{id}", ownerRoute, s.requireOfflineControl(s.handleDeletePack))
	// Clearing an owner-scoped cache scope is the caller's own business; the
	// shared scopes are the operator's. The handler tells them apart.
	t.delete("/offline/cache", ownerRoute, s.requireOfflineControl(s.handleClearCache))
	if s.broom != nil {
		t.get("/offline/routing", publicRoute, s.requireOfflineRead(s.handleBroomStatus))
		t.get("/offline/routing/regions", publicRoute, s.requireOfflineRead(s.handleBroomRegions))
		t.post("/offline/routing/suggest", publicRoute, s.requireOfflineRead(s.handleBroomSuggest))
		// A plan downloads nothing; every user browsing regions needs it.
		t.post("/offline/routing/plan", ownerRoute, s.requireOfflineControl(s.handleBroomPlan))
		t.post("/offline/routing/profile", ownerRoute, s.requireOfflineControl(s.handleSessionProfile))
		t.post("/offline/routing/profile/release", ownerRoute, s.requireOfflineControl(s.handleReleaseSessionProfile))
		// One active routing region per server: choosing it is an operator's
		// decision, as is what stays on disk.
		t.post("/offline/routing/prepare", operatorRoute, s.requireOfflineControl(s.handleBroomPrepare))
		t.post("/offline/routing/cancel", operatorRoute, s.requireOfflineControl(s.handleBroomCancel))
		t.post("/offline/routing/pin", operatorRoute, s.requireOfflineControl(s.handleBroomPin))
		t.post("/offline/routing/prune", operatorRoute, s.requireOfflineControl(s.handleBroomPrune))
	}
	t.handle(http.MethodOptions, "/offline/*", publicRoute, s.handleOfflineOptions)
	t.get("/config", publicRoute, s.handleConfig)
	if s.mcpBrowser != nil {
		r.Mount("/mcp/browser", http.StripPrefix("/mcp/browser", s.mcpBrowser))
	}
	t.handle(http.MethodOptions, "/*", publicRoute, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	t.get("/*", publicRoute, s.assetHandler().ServeHTTP)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "Not found")
	})
	return r
}
