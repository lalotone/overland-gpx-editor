package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

type rasterAdapter struct {
	policy   *providerPolicy
	template string
	maxZoom  int
	ext      string
}

func newRasterAdapters() (map[string]*rasterAdapter, error) {
	type definition struct {
		name        string
		base        string
		template    string
		maxZoom     int
		fresh       time.Duration
		stale       time.Duration
		retention   time.Duration
		staleError  bool
		concurrency int
	}
	definitions := []definition{
		{name: "osm", base: "https://tile.openstreetmap.org", template: "https://tile.openstreetmap.org/{z}/{x}/{y}.png", maxZoom: 19, fresh: 7 * 24 * time.Hour, stale: -1, retention: 30 * 24 * time.Hour, concurrency: 2},
		{name: "opentopo", base: "https://a.tile.opentopomap.org", template: "https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png", maxZoom: 17, fresh: 24 * time.Hour, stale: 7 * 24 * time.Hour, retention: 30 * 24 * time.Hour, staleError: true, concurrency: 2},
		{name: "cyclosm", base: "https://a.tile-cyclosm.openstreetmap.fr", template: "https://{s}.tile-cyclosm.openstreetmap.fr/cyclosm/{z}/{x}/{y}.png", maxZoom: 19, fresh: 72 * time.Hour, stale: -1, retention: 72 * time.Hour, concurrency: 2},
	}
	out := make(map[string]*rasterAdapter, len(definitions))
	for _, definition := range definitions {
		base, err := parseProviderURL(definition.name, definition.base)
		if err != nil {
			return nil, err
		}
		policy := newProviderPolicy("map-"+definition.name, "maps-"+definition.name, base, definition.fresh, definition.stale, definition.retention, definition.staleError, 2<<20, []string{"image/png", "image/jpeg", "image/webp"}, newConcurrentRateGroup(0, definition.concurrency), false)
		if definition.name == "opentopo" {
			for _, host := range []string{"a.tile.opentopomap.org", "b.tile.opentopomap.org", "c.tile.opentopomap.org"} {
				policy.approvedHosts[host] = struct{}{}
			}
		}
		if definition.name == "cyclosm" {
			for _, host := range []string{"a.tile-cyclosm.openstreetmap.fr", "b.tile-cyclosm.openstreetmap.fr", "c.tile-cyclosm.openstreetmap.fr"} {
				policy.approvedHosts[host] = struct{}{}
			}
		}
		out[definition.name] = &rasterAdapter{policy: policy, template: definition.template, maxZoom: definition.maxZoom, ext: "png"}
	}
	return out, nil
}

func parseTileCoordinate(raw string, max int) (int, error) {
	if raw == "" || len(raw) > 10 {
		return 0, errors.New("invalid tile coordinate")
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > max {
		return 0, errors.New("invalid tile coordinate")
	}
	return value, nil
}

func validateTile(zRaw, xRaw, yRaw string, maxZoom int) (int, int, int, error) {
	z, err := parseTileCoordinate(zRaw, maxZoom)
	if err != nil {
		return 0, 0, 0, err
	}
	bound := (1 << z) - 1
	x, err := parseTileCoordinate(xRaw, bound)
	if err != nil {
		return 0, 0, 0, err
	}
	y, err := parseTileCoordinate(yRaw, bound)
	if err != nil {
		return 0, 0, 0, err
	}
	return z, x, y, nil
}

func (s *Server) protectOutboundResource(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
		if site == "cross-site" {
			writeError(w, http.StatusForbidden, "Cross-site resource relay refused")
			return
		}
		if site == "" && r.Header.Get("Origin") == "" && !remoteIsLoopback(r.RemoteAddr) {
			writeError(w, http.StatusForbidden, "Resource requests require a same-origin or loopback client")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.isTrustedResourceOrigin(origin, r) {
			writeError(w, http.StatusForbidden, "Cross-origin resource relay refused")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleRasterMap(w http.ResponseWriter, r *http.Request) {
	layer := chi.URLParam(r, "layer")
	adapter := s.rasterMaps[layer]
	if adapter == nil {
		writeError(w, http.StatusNotFound, "Unknown raster layer")
		return
	}
	yRaw := chi.URLParam(r, "y")
	ext := path.Ext(yRaw)
	if ext != "."+adapter.ext {
		writeError(w, http.StatusBadRequest, "Invalid raster extension")
		return
	}
	yRaw = strings.TrimSuffix(yRaw, ext)
	z, x, y, err := validateTile(chi.URLParam(r, "z"), chi.URLParam(r, "x"), yRaw, adapter.maxZoom)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	subdomain := string("abc"[(x+y)%3])
	endpoint := strings.NewReplacer("{s}", subdomain, "{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x), "{y}", strconv.Itoa(y)).Replace(adapter.template)
	response, err := s.outbound.do(r.Context(), cachedRequest{
		policy: adapter.policy, method: http.MethodGet, url: endpoint,
		params: fmt.Sprintf("%d/%d/%d.%s", z, x, y, adapter.ext), cacheable: true,
		headers:  map[string]string{"Accept": "image/avif,image/webp,image/png,image/*"},
		validate: validateImageResponse,
	})
	if err != nil {
		writeOutboundError(w, err, adapter.policy.scope)
		return
	}
	writeCachedResponse(w, response)
}

type openFreeMapManager struct {
	server    *Server
	base      *url.URL
	policy    *providerPolicy
	allowBulk bool

	mu            sync.RWMutex
	active        bool
	style         []byte
	sources       map[string][]byte
	tiles         map[string]string
	rasters       map[string]string
	rasterMaxZoom map[string]int
	glyphs        string
	sprite        string
	primarySource string
	coreKeys      []string
	durableCore   bool
	generation    string
	cacheResponse cachedResponse
	fontStacks    []string
}

type openFreeMapGeneration struct {
	Schema            int               `json:"schema"`
	SourceFingerprint string            `json:"sourceFingerprint"`
	ID                string            `json:"id"`
	Style             []byte            `json:"style"`
	Sources           map[string][]byte `json:"sources"`
	Tiles             map[string]string `json:"tiles"`
	Rasters           map[string]string `json:"rasters"`
	Glyphs            string            `json:"glyphs,omitempty"`
	Sprite            string            `json:"sprite,omitempty"`
	PrimarySource     string            `json:"primarySource,omitempty"`
	CoreKeys          []string          `json:"coreKeys"`
	CachedAt          time.Time         `json:"cachedAt"`
	FontStacks        []string          `json:"fontStacks,omitempty"`
}

func newOpenFreeMapManager(server *Server, base *url.URL, allowBulk bool) *openFreeMapManager {
	policy := newProviderPolicy("openfreemap-compatible", "maps-openfreemap", base, 24*time.Hour, 30*24*time.Hour, 90*24*time.Hour, true, 16<<20,
		[]string{"application/json", "application/vnd.mapbox-vector-tile", "application/x-protobuf", "application/octet-stream", "image/png", "image/jpeg", "image/webp"}, newConcurrentRateGroup(0, 4), allowBulk)
	m := &openFreeMapManager{server: server, base: base, policy: policy, allowBulk: allowBulk}
	m.loadGeneration()
	return m
}

func (m *openFreeMapManager) generationPath() string {
	if !m.server.cache.writable {
		return ""
	}
	return filepath.Join("v1", "openfreemap-generation.json")
}

func (m *openFreeMapManager) loadGeneration() {
	path := m.generationPath()
	if path == "" {
		return
	}
	raw, err := readFileLimitAt(m.server.cache.root, path, 4<<20)
	if err != nil {
		return
	}
	var generation openFreeMapGeneration
	if json.Unmarshal(raw, &generation) != nil || generation.Schema != cacheSchemaVersion ||
		generation.SourceFingerprint != m.policy.sourceFingerprint || !validCacheKey(generation.ID) || !json.Valid(generation.Style) {
		return
	}
	if !m.validGenerationResources(generation) {
		return
	}
	now := time.Now().UTC()
	for _, key := range generation.CoreKeys {
		if !validCacheKey(key) {
			return
		}
		entry, ok := m.server.cache.get(m.policy.scope, key)
		if !ok || !staleAllowed(entry.Meta, now) {
			return
		}
	}
	for _, font := range generation.FontStacks {
		if !validFontStack(font) {
			return
		}
	}
	m.active = true
	m.style = generation.Style
	m.sources = generation.Sources
	m.tiles = generation.Tiles
	m.rasters = generation.Rasters
	m.rasterMaxZoom = rewrittenRasterMaxZoom(generation.Style, generation.Sources)
	m.glyphs = generation.Glyphs
	m.sprite = generation.Sprite
	m.primarySource = generation.PrimarySource
	m.coreKeys = generation.CoreKeys
	m.durableCore = true
	m.generation = generation.ID
	m.cacheResponse = cachedResponse{State: "hit", Meta: cacheMetadata{FetchedAt: generation.CachedAt}}
	m.fontStacks = generation.FontStacks
}

func (m *openFreeMapManager) validGenerationResources(generation openFreeMapGeneration) bool {
	if generation.PrimarySource != "" {
		if _, ok := generation.Sources[generation.PrimarySource]; !ok {
			return false
		}
	}
	for id, body := range generation.Sources {
		if !safeResourceID(id) || !json.Valid(body) {
			return false
		}
	}
	for _, resources := range []map[string]string{generation.Tiles, generation.Rasters} {
		for id, template := range resources {
			if !safeResourceID(id) || !validResourceTemplate(m, template) {
				return false
			}
		}
	}
	if generation.Glyphs != "" && !validResourceTemplate(m, generation.Glyphs) {
		return false
	}
	if generation.Sprite != "" && !validResourceTemplate(m, generation.Sprite) {
		return false
	}
	return true
}

func validResourceTemplate(m *openFreeMapManager, template string) bool {
	resolved, err := m.resolve(m.base, template)
	return err == nil && resourceTemplateString(resolved) == template
}

func (m *openFreeMapManager) persistGeneration(generation openFreeMapGeneration) error {
	path := m.generationPath()
	if path == "" {
		return errors.New("persistent cache is disabled")
	}
	raw, err := json.Marshal(generation)
	if err != nil {
		return err
	}
	if len(raw) > maxMapGenerationBytes {
		return errors.New("OpenFreeMap generation exceeds storage limit")
	}
	return atomicWriteFileAt(m.server.cache.root, m.server.cache.tmpRel, path, raw)
}

func (m *openFreeMapManager) styleURL() string {
	if strings.HasSuffix(m.base.Path, ".json") || strings.Contains(m.base.Path, "/styles/") {
		return m.base.String()
	}
	return providerEndpoint(m.base, "/styles/liberty")
}

func (m *openFreeMapManager) activate(ctx context.Context) {
	styleURL := m.styleURL()
	response, err := m.fetch(ctx, styleURL, "style", []string{"application/json"})
	if err != nil {
		return
	}
	var style map[string]any
	if json.Unmarshal(response.Body, &style) != nil {
		return
	}
	generationID := checksum(response.Body)
	styleBase, _ := url.Parse(styleURL)
	coreKeys := make([]string, 0, 8)
	durableCore := response.State != "bypass"

	candidateSources := make(map[string][]byte)
	candidateTiles := make(map[string]string)
	candidateRasters := make(map[string]string)
	sources, _ := style["sources"].(map[string]any)
	for sourceID, raw := range sources {
		if !safeResourceID(sourceID) {
			return
		}
		source, ok := raw.(map[string]any)
		if !ok {
			return
		}
		if rawURL, ok := source["url"].(string); ok && rawURL != "" {
			resolved, err := m.resolve(styleBase, rawURL)
			if err != nil {
				return
			}
			tileJSON, err := m.fetch(ctx, resolved.String(), generationID+":source:"+sourceID, []string{"application/json"})
			if err != nil {
				return
			}
			coreKeys = append(coreKeys, tileJSON.Key)
			durableCore = durableCore && tileJSON.State != "bypass"
			var manifest map[string]any
			if json.Unmarshal(tileJSON.Body, &manifest) != nil {
				return
			}
			if !m.rewriteTiles(manifest, resolved, sourceID, candidateTiles, candidateRasters) {
				return
			}
			candidateSources[sourceID], _ = json.Marshal(manifest)
			source["url"] = "/map/openfreemap/source/" + url.PathEscape(sourceID) + ".json"
		}
		if !m.rewriteTiles(source, styleBase, sourceID, candidateTiles, candidateRasters) {
			return
		}
	}

	glyphs := ""
	if raw, ok := style["glyphs"].(string); ok && raw != "" {
		resolved, err := m.resolve(styleBase, raw)
		if err != nil {
			return
		}
		glyphs = resourceTemplateString(resolved)
		style["glyphs"] = "/map/openfreemap/glyphs/{fontstack}/{range}.pbf"
	}
	sprite := ""
	if raw, ok := style["sprite"].(string); ok && raw != "" {
		resolved, err := m.resolve(styleBase, raw)
		if err != nil {
			return
		}
		sprite = resolved.String()
		for _, suffix := range []string{".json", ".png", "@2x.json", "@2x.png"} {
			accepted := []string{"image/png"}
			if strings.HasSuffix(suffix, ".json") {
				accepted = []string{"application/json"}
			}
			spriteResponse, err := m.fetch(ctx, sprite+suffix, generationID+":sprite:"+suffix, accepted)
			if err != nil {
				return
			}
			coreKeys = append(coreKeys, spriteResponse.Key)
			durableCore = durableCore && spriteResponse.State != "bypass"
		}
		style["sprite"] = "/map/openfreemap/sprite"
	}
	rewritten, err := json.Marshal(style)
	if err != nil {
		return
	}
	primary := ""
	fontStacks := styleFontStacks(style)
	for sourceID := range candidateSources {
		if primary == "" || sourceID < primary {
			primary = sourceID
		}
	}
	generation := openFreeMapGeneration{
		Schema: cacheSchemaVersion, SourceFingerprint: m.policy.sourceFingerprint, ID: generationID,
		Style: rewritten, Sources: candidateSources, Tiles: candidateTiles, Rasters: candidateRasters,
		Glyphs: glyphs, Sprite: sprite, PrimarySource: primary, CoreKeys: coreKeys, CachedAt: response.Meta.FetchedAt,
		FontStacks: fontStacks,
	}
	if durableCore && m.persistGeneration(generation) != nil {
		durableCore = false
	}
	m.mu.Lock()
	m.active = true
	m.style = rewritten
	m.sources = candidateSources
	m.tiles = candidateTiles
	m.rasters = candidateRasters
	m.rasterMaxZoom = rewrittenRasterMaxZoom(rewritten, candidateSources)
	m.glyphs = glyphs
	m.sprite = sprite
	m.primarySource = primary
	m.coreKeys = coreKeys
	m.durableCore = durableCore
	m.generation = generationID
	m.cacheResponse = response
	m.fontStacks = fontStacks
	m.mu.Unlock()
}

func styleFontStacks(style map[string]any) []string {
	seen := make(map[string]struct{})
	layers, _ := style["layers"].([]any)
	for _, rawLayer := range layers {
		layer, _ := rawLayer.(map[string]any)
		layout, _ := layer["layout"].(map[string]any)
		collectFontStacks(layout["text-font"], seen)
	}
	fonts := make([]string, 0, len(seen))
	for font := range seen {
		fonts = append(fonts, font)
	}
	sort.Strings(fonts)
	return fonts
}

func collectFontStacks(value any, seen map[string]struct{}) {
	values, ok := value.([]any)
	if !ok || len(values) == 0 {
		return
	}
	allStrings := true
	fonts := make([]string, len(values))
	for i, value := range values {
		font, ok := value.(string)
		if !ok || !validFontStack(font) {
			allStrings = false
			break
		}
		fonts[i] = font
	}
	if allStrings && !fontExpressionOperator(fonts[0]) {
		stack := strings.Join(fonts, ",")
		if len(stack) <= 200 {
			seen[stack] = struct{}{}
		}
		return
	}
	for _, nested := range values[1:] {
		collectFontStacks(nested, seen)
	}
}

func validFontStack(font string) bool {
	return font != "" && len(font) <= 200 && !strings.ContainsAny(font, "/\\\x00{}")
}

func fontExpressionOperator(value string) bool {
	switch value {
	case "literal", "case", "match", "coalesce", "get", "concat", "step", "interpolate":
		return true
	default:
		return false
	}
}

func (m *openFreeMapManager) fetch(ctx context.Context, endpoint, params string, accepted []string) (cachedResponse, error) {
	return m.fetchWithAdmission(ctx, endpoint, params, accepted, nil)
}

func (m *openFreeMapManager) fetchWithAdmission(ctx context.Context, endpoint, params string, accepted []string, admit func(int64) error) (cachedResponse, error) {
	copyPolicy := *m.policy
	copyPolicy.contentTypes = accepted
	return m.server.outbound.do(ctx, cachedRequest{
		policy: &copyPolicy, method: http.MethodGet, url: endpoint, params: params, cacheable: true,
		headers: map[string]string{"Accept": strings.Join(accepted, ",")}, validate: openFreeMapValidator(params, accepted),
		admit: admit, cancelWithCaller: admit != nil,
	})
}

func openFreeMapValidator(params string, accepted []string) func([]byte) error {
	if containsString(accepted, "application/json") {
		return func(body []byte) error {
			var object map[string]json.RawMessage
			if json.Unmarshal(body, &object) != nil {
				return errors.New("OpenFreeMap-compatible source returned an invalid JSON object")
			}
			if params == "style" {
				var version int
				var sources map[string]json.RawMessage
				var layers []json.RawMessage
				if json.Unmarshal(object["version"], &version) != nil || version != 8 ||
					json.Unmarshal(object["sources"], &sources) != nil || json.Unmarshal(object["layers"], &layers) != nil {
					return errors.New("OpenFreeMap-compatible source returned an invalid style")
				}
			}
			if strings.Contains(params, ":source:") {
				var tiles []string
				if json.Unmarshal(object["tiles"], &tiles) != nil || len(tiles) == 0 {
					return errors.New("OpenFreeMap-compatible source returned invalid TileJSON")
				}
			}
			return nil
		}
	}
	if containsString(accepted, "image/png") {
		return validateImageResponse
	}
	return func(body []byte) error {
		if len(body) == 0 {
			return errors.New("OpenFreeMap-compatible source returned an empty resource")
		}
		return nil
	}
}

func validateImageResponse(body []byte) error {
	png := []byte("\x89PNG\r\n\x1a\n")
	jpeg := []byte("\xff\xd8\xff")
	if bytes.HasPrefix(body, png) || bytes.HasPrefix(body, jpeg) ||
		(len(body) >= 12 && string(body[:4]) == "RIFF" && string(body[8:12]) == "WEBP") {
		return nil
	}
	return errors.New("map provider returned an invalid image")
}

func (m *openFreeMapManager) resolve(base *url.URL, raw string) (*url.URL, error) {
	ref, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	resolved := base.ResolveReference(ref)
	if !strings.EqualFold(resolved.Scheme, m.base.Scheme) || !strings.EqualFold(resolved.Host, m.base.Host) || resolved.User != nil {
		return nil, errors.New("style resource left approved origin")
	}
	return resolved, nil
}

func (m *openFreeMapManager) rewriteTiles(container map[string]any, base *url.URL, sourceID string, vectors map[string]string, rasters map[string]string) bool {
	rawTiles, ok := container["tiles"].([]any)
	if !ok {
		return true
	}
	kind, _ := container["type"].(string)
	rewritten := make([]any, 0, len(rawTiles))
	for index, raw := range rawTiles {
		template, ok := raw.(string)
		if !ok {
			return false
		}
		resolved, err := m.resolve(base, template)
		if err != nil {
			return false
		}
		id := sourceID + "-" + strconv.Itoa(index)
		if kind == "raster" || !strings.Contains(strings.ToLower(resolved.Path), ".pbf") {
			rasters[id] = resourceTemplateString(resolved)
			ext := path.Ext(resolved.Path)
			if ext == "" {
				ext = ".png"
			}
			rewritten = append(rewritten, "/map/openfreemap/raster/"+url.PathEscape(id)+"/{z}/{x}/{y}"+ext)
		} else {
			vectors[id] = resourceTemplateString(resolved)
			rewritten = append(rewritten, "/map/openfreemap/tiles/"+url.PathEscape(id)+"/{z}/{x}/{y}.pbf")
		}
	}
	container["tiles"] = rewritten
	return true
}

func resourceTemplateString(value *url.URL) string {
	return strings.NewReplacer("%7B", "{", "%7D", "}", "%7b", "{", "%7d", "}").Replace(value.String())
}

func rewrittenRasterMaxZoom(style []byte, sources map[string][]byte) map[string]int {
	zooms := make(map[string]int)
	readSource := func(raw json.RawMessage) {
		var source struct {
			Tiles   []string `json:"tiles"`
			MaxZoom *int     `json:"maxzoom"`
		}
		if json.Unmarshal(raw, &source) != nil {
			return
		}
		maxZoom := 19
		if source.MaxZoom != nil {
			maxZoom = max(0, min(*source.MaxZoom, 19))
		}
		const prefix = "/map/openfreemap/raster/"
		for _, tile := range source.Tiles {
			if !strings.HasPrefix(tile, prefix) {
				continue
			}
			id, _, _ := strings.Cut(strings.TrimPrefix(tile, prefix), "/")
			if decoded, err := url.PathUnescape(id); err == nil && safeResourceID(decoded) {
				zooms[decoded] = maxZoom
			}
		}
	}
	var document struct {
		Sources map[string]json.RawMessage `json:"sources"`
	}
	if json.Unmarshal(style, &document) == nil {
		for _, source := range document.Sources {
			readSource(source)
		}
	}
	for _, source := range sources {
		readSource(source)
	}
	return zooms
}

func safeResourceID(value string) bool {
	if value == "" || len(value) > 80 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func (m *openFreeMapManager) isActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

func (m *openFreeMapManager) handleStyle(w http.ResponseWriter, _ *http.Request) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.active {
		writeError(w, http.StatusServiceUnavailable, "Configured map style is not available")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	setCacheHeaders(w, m.cacheResponse)
	w.Write(m.style)
}

func (m *openFreeMapManager) handleSource(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(chi.URLParam(r, "source"), ".json")
	m.mu.RLock()
	if id == "" {
		id = m.primarySource
	}
	body := m.sources[id]
	cacheResponse := m.cacheResponse
	m.mu.RUnlock()
	if body == nil {
		writeError(w, http.StatusNotFound, "Unknown map source")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	setCacheHeaders(w, cacheResponse)
	w.Write(body)
}

func (m *openFreeMapManager) handleTile(w http.ResponseWriter, r *http.Request, raster bool) {
	id := chi.URLParam(r, "source")
	m.mu.RLock()
	if id == "" {
		id = m.primarySource + "-0"
	}
	template := m.tiles[id]
	generation := m.generation
	if raster {
		template = m.rasters[id]
	}
	m.mu.RUnlock()
	if template == "" {
		writeError(w, http.StatusNotFound, "Unknown map source")
		return
	}
	yRaw := chi.URLParam(r, "y")
	ext := path.Ext(yRaw)
	yRaw = strings.TrimSuffix(yRaw, ext)
	if (!raster && ext != ".pbf") || (raster && ext != path.Ext(strings.Split(template, "?")[0]) && path.Ext(strings.Split(template, "?")[0]) != "") {
		writeError(w, http.StatusBadRequest, "Invalid map extension")
		return
	}
	z, x, y, err := validateTile(chi.URLParam(r, "z"), chi.URLParam(r, "x"), yRaw, 19)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	endpoint := strings.NewReplacer("{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x), "{y}", strconv.Itoa(y)).Replace(template)
	accepted := []string{"application/vnd.mapbox-vector-tile", "application/x-protobuf", "application/octet-stream"}
	if raster {
		accepted = []string{"image/png", "image/jpeg", "image/webp"}
	}
	response, err := m.fetch(r.Context(), endpoint, fmt.Sprintf("%s:resource:%s:%d/%d/%d", generation, id, z, x, y), accepted)
	if err != nil {
		writeOutboundError(w, err, m.policy.scope)
		return
	}
	writeCachedResponse(w, response)
}

var glyphRangePattern = regexp.MustCompile(`^(\d+)-(\d+)$`)

func (m *openFreeMapManager) handleGlyph(w http.ResponseWriter, r *http.Request) {
	font := chi.URLParam(r, "fontstack")
	rangeValue := strings.TrimSuffix(chi.URLParam(r, "range"), ".pbf")
	matches := glyphRangePattern.FindStringSubmatch(rangeValue)
	start, _ := strconv.Atoi(valueAt(matches, 1))
	end, _ := strconv.Atoi(valueAt(matches, 2))
	if font == "" || len(font) > 200 || strings.ContainsAny(font, `/\\\x00`) || len(matches) != 3 || start%256 != 0 || end != start+255 || end > 65535 {
		writeError(w, http.StatusBadRequest, "Invalid glyph path")
		return
	}
	m.mu.RLock()
	template := m.glyphs
	generation := m.generation
	m.mu.RUnlock()
	if template == "" {
		writeError(w, http.StatusNotFound, "Glyphs are not configured")
		return
	}
	endpoint := strings.NewReplacer("{fontstack}", url.PathEscape(font), "{range}", rangeValue).Replace(template)
	response, err := m.fetch(r.Context(), endpoint, generation+":glyph:"+font+":"+rangeValue, []string{"application/x-protobuf", "application/octet-stream", "application/vnd.mapbox-vector-tile"})
	if err != nil {
		writeOutboundError(w, err, m.policy.scope)
		return
	}
	writeCachedResponse(w, response)
}

func valueAt(values []string, index int) string {
	if index >= 0 && index < len(values) {
		return values[index]
	}
	return ""
}

func (m *openFreeMapManager) handleSprite(w http.ResponseWriter, r *http.Request) {
	variant := chi.URLParam(r, "variant")
	if variant != "sprite.json" && variant != "sprite.png" && variant != "sprite@2x.json" && variant != "sprite@2x.png" {
		writeError(w, http.StatusNotFound, "Unknown sprite resource")
		return
	}
	suffix := strings.TrimPrefix(variant, "sprite")
	m.mu.RLock()
	base := m.sprite
	generation := m.generation
	m.mu.RUnlock()
	accepted := []string{"image/png"}
	if strings.HasSuffix(variant, ".json") {
		accepted = []string{"application/json"}
	}
	response, err := m.fetch(r.Context(), base+suffix, generation+":sprite:"+suffix, accepted)
	if err != nil {
		writeOutboundError(w, err, m.policy.scope)
		return
	}
	writeCachedResponse(w, response)
}
