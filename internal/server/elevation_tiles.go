package server

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png" // Terrarium tiles are PNG; decoder registration only.
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

/*
 * Elevation from terrain-RGB tiles.
 *
 * Instead of asking a service for one elevation per point, this reads the
 * raster directly: Terrarium tiles encode metres in the pixel colour, so a
 * tile is a 256x256 grid of ground heights. Two things follow from that.
 *
 * The data is ~30 m where the underlying sources are (SRTM, NED and friends)
 * against Copernicus 90 m from the fallback API, which matters most on
 * exactly the steep ground this app is used for — a 90 m cell averages across
 * terrain that can move hundreds of metres.
 *
 * And once a tile is cached there is no per-point cost at all, so the 6000
 * point ceiling and the interpolation it forces do not apply, and a cached
 * corridor keeps working with no network.
 *
 * The price is bandwidth: tiles are ~100 KB each and a 200 km route corridor
 * needs roughly 77 of them at zoom 13.
 */

// Terrarium tiles from the AWS Open Data terrain set: public, keyless.
const defaultTileURL = "https://s3.amazonaws.com/elevation-tiles-prod/terrarium/{z}/{x}/{y}.png"

const (
	tileSize = 256
	// ~14 m/px at 42° latitude. Zoom 14 doubles the resolution and quadruples
	// the tiles; 12 halves the bandwidth and blurs exactly what we came for.
	defaultTileZoom = 13
	// 256x256 float32 is 256 KB a tile, so this caps the cache at ~32 MB.
	maxCachedTiles = 128
	// Politeness towards a shared public bucket, and a cap on how many tiles
	// one long route can pull at once.
	tileFetchConcurrency = 6
)

// tileDataset is reported back to callers so the source of a number is never
// a guess.
const tileDataset = "terrarium30m"

type tileKey struct{ z, x, y int }

func (k tileKey) path() string { return fmt.Sprintf("%d/%d/%d.png", k.z, k.x, k.y) }

// tileGrid is one decoded tile: elevation in metres, row-major.
type tileGrid struct {
	ele []float32
}

func (g *tileGrid) at(x, y int) float64 { return float64(g.ele[y*tileSize+x]) }

// tileFetch lets callers that want the same tile wait on one request rather
// than each starting their own — a batch of 100 points usually lands in a
// handful of tiles.
type tileFetch struct {
	done chan struct{}
	grid *tileGrid
	err  error
}

type tileStore struct {
	url          string
	zoom         int
	cacheDir     string
	cacheRoot    *os.Root
	cacheErr     error
	maxDiskBytes int64
	client       *http.Client
	userAgent    string
	sem          chan struct{}
	offline      bool
	ctx          context.Context
	wg           *sync.WaitGroup

	mu       sync.Mutex
	lru      *list.List // front = most recently used, values are lruEntry
	index    map[tileKey]*list.Element
	inflight map[tileKey]*tileFetch

	prefetch prefetchState

	diskMu    sync.Mutex
	diskBytes int64
	diskFiles map[string]tileDiskFile
}

type tileDiskFile struct {
	key        tileKey
	size       int64
	lastAccess time.Time
}

type lruEntry struct {
	key  tileKey
	grid *tileGrid
}

func newTileStore(url string, zoom int, cacheDir string, client *http.Client) *tileStore {
	return newTileStoreWithQuota(url, zoom, cacheDir, defaultElevationTileCacheBytes, client)
}

func newTileStoreWithQuota(url string, zoom int, cacheDir string, maxDiskBytes int64, client *http.Client) *tileStore {
	if url == "" {
		url = defaultTileURL
	}
	if maxDiskBytes <= 0 {
		maxDiskBytes = defaultElevationTileCacheBytes
	}
	if zoom <= 0 {
		zoom = defaultTileZoom
	}
	store := &tileStore{
		url:          url,
		zoom:         zoom,
		cacheDir:     cacheDir,
		client:       client,
		userAgent:    userAgent,
		sem:          make(chan struct{}, tileFetchConcurrency),
		lru:          list.New(),
		index:        make(map[tileKey]*list.Element),
		inflight:     make(map[tileKey]*tileFetch),
		maxDiskBytes: maxDiskBytes,
		diskFiles:    make(map[string]tileDiskFile),
		ctx:          context.Background(),
		wg:           &sync.WaitGroup{},
	}
	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			store.cacheErr = err
			return store
		}
		root, err := os.OpenRoot(cacheDir)
		if err != nil {
			store.cacheErr = err
			return store
		}
		store.cacheRoot = root
		if err := root.Chmod(".", 0o700); err != nil {
			root.Close()
			store.cacheRoot = nil
			store.cacheErr = err
			return store
		}
		if err := store.loadDiskIndex(); err != nil {
			root.Close()
			store.cacheRoot = nil
			store.cacheErr = err
		}
	}
	return store
}

func (s *tileStore) configureLifecycle(offline bool, ctx context.Context, wg *sync.WaitGroup) {
	s.offline = offline
	s.ctx = ctx
	s.wg = wg
}

func (s *tileStore) close() {
	s.prefetch.mu.Lock()
	if s.prefetch.cancel != nil {
		s.prefetch.cancel()
	}
	s.prefetch.mu.Unlock()
}

func (s *tileStore) closeCache() error {
	if s.cacheRoot == nil {
		return nil
	}
	return s.cacheRoot.Close()
}

/* -- Sampling --------------------------------------------------------- */

// lookup returns one elevation per point. A nil entry means the tile covering
// that point had no data, never a zero — a zero reads as sea level.
func (s *tileStore) lookup(ctx context.Context, points []point) ([]*float64, error) {
	out := make([]*float64, len(points))
	for i, p := range points {
		v, err := s.sample(ctx, p)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// sample reads one point, interpolating bilinearly between the four
// surrounding posts. Nearest-neighbour would step the profile in ~14 m
// terraces, which the slope maths would then read as a wall.
func (s *tileStore) sample(ctx context.Context, p point) (*float64, error) {
	worldPx, worldPy := s.project(p)

	// Pixel values sit at pixel centres, half a pixel in from the corner.
	fx, fy := worldPx-0.5, worldPy-0.5
	x0, y0 := math.Floor(fx), math.Floor(fy)
	dx, dy := fx-x0, fy-y0

	corners := [4]struct {
		ox, oy int
		w      float64
	}{
		{0, 0, (1 - dx) * (1 - dy)},
		{1, 0, dx * (1 - dy)},
		{0, 1, (1 - dx) * dy},
		{1, 1, dx * dy},
	}

	var sum float64
	for _, c := range corners {
		v, ok, err := s.pixel(ctx, int(x0)+c.ox, int(y0)+c.oy)
		if err != nil {
			return nil, err
		}
		// Partial coverage would weight a real reading against nothing, so a
		// missing corner makes the whole sample unknown.
		if !ok {
			return nil, nil
		}
		sum += v * c.w
	}
	return &sum, nil
}

// project converts a coordinate to fractional pixel coordinates in the whole
// world at this zoom (Web Mercator).
func (s *tileStore) project(p point) (px, py float64) {
	n := math.Exp2(float64(s.zoom))
	lat := math.Max(-85.05112878, math.Min(85.05112878, p.lat))
	latRad := lat * math.Pi / 180
	px = (p.lon + 180) / 360 * n * tileSize
	py = (1 - math.Log(math.Tan(latRad)+1/math.Cos(latRad))/math.Pi) / 2 * n * tileSize
	return
}

// pixel reads one post by global pixel coordinate, fetching its tile if
// needed. ok is false when the tile is outside coverage.
func (s *tileStore) pixel(ctx context.Context, gx, gy int) (float64, bool, error) {
	world := int(math.Exp2(float64(s.zoom))) * tileSize
	// Longitude wraps; latitude does not.
	gx = ((gx % world) + world) % world
	if gy < 0 || gy >= world {
		return 0, false, nil
	}

	key := tileKey{z: s.zoom, x: gx / tileSize, y: gy / tileSize}
	grid, err := s.grid(ctx, key)
	if err != nil {
		return 0, false, err
	}
	if grid == nil {
		return 0, false, nil
	}
	return grid.at(gx%tileSize, gy%tileSize), true, nil
}

/* -- Tile acquisition ------------------------------------------------- */

// grid returns a decoded tile from memory, disk or the network, in that
// order. A nil grid with a nil error means the tile is not covered.
func (s *tileStore) grid(ctx context.Context, key tileKey) (*tileGrid, error) {
	s.mu.Lock()
	if el, ok := s.index[key]; ok {
		s.lru.MoveToFront(el)
		grid := el.Value.(*lruEntry).grid
		s.mu.Unlock()
		s.touchDisk(key)
		return grid, nil
	}
	if f, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.grid, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &tileFetch{done: make(chan struct{})}
	s.inflight[key] = f
	s.mu.Unlock()

	f.grid, f.err = s.load(ctx, key)
	close(f.done)

	s.mu.Lock()
	delete(s.inflight, key)
	if f.err == nil && f.grid != nil {
		s.index[key] = s.lru.PushFront(&lruEntry{key: key, grid: f.grid})
		for s.lru.Len() > maxCachedTiles {
			oldest := s.lru.Back()
			s.lru.Remove(oldest)
			delete(s.index, oldest.Value.(*lruEntry).key)
		}
	}
	s.mu.Unlock()

	return f.grid, f.err
}

func (s *tileStore) load(ctx context.Context, key tileKey) (*tileGrid, error) {
	if raw, ok := s.readDisk(key); ok {
		return decodeTerrarium(raw)
	}
	if s.offline {
		return nil, &offlineMissError{Scope: "elevation-tiles"}
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	url := strings.NewReplacer(
		"{z}", fmt.Sprint(key.z),
		"{x}", fmt.Sprint(key.x),
		"{y}", fmt.Sprint(key.y),
	).Replace(s.url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Referer", "https://github.com/lalotone/overland-gpx-editor")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, fmt.Errorf("terrain tile content encoding %q is not accepted", encoding)
	}

	// Outside coverage. Not an error: the caller reports no data for those
	// points and the rest of the route is unaffected.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("terrain tile %s returned %d", key.path(), resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxElevationBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxElevationBodyBytes {
		return nil, errors.New("terrain tile exceeds response limit")
	}
	grid, err := decodeTerrarium(raw)
	if err != nil {
		return nil, err
	}
	if err := s.writeDisk(key, raw); err != nil {
		return nil, err
	}
	return grid, nil
}

// decodeTerrarium turns a terrain-RGB tile into metres:
//
//	elevation = (R * 256 + G + B / 256) - 32768
func decodeTerrarium(raw []byte) (*tileGrid, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("terrain tile is not a readable image: %w", err)
	}
	if format != "png" {
		return nil, fmt.Errorf("terrain tile format is %q, want png", format)
	}
	if config.Width != tileSize || config.Height != tileSize {
		return nil, fmt.Errorf("terrain tile is %dx%d, want %dx%d", config.Width, config.Height, tileSize, tileSize)
	}
	img, decodedFormat, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("terrain tile is not a readable PNG: %w", err)
	}
	if decodedFormat != "png" {
		return nil, fmt.Errorf("terrain tile decoded as %q, want png", decodedFormat)
	}
	b := img.Bounds()

	grid := &tileGrid{ele: make([]float32, tileSize*tileSize)}
	for y := 0; y < tileSize; y++ {
		for x := 0; x < tileSize; x++ {
			// RGBA() returns 16-bit channels; the tile is 8-bit per channel.
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			grid.ele[y*tileSize+x] = float32(
				float64(r>>8)*256 + float64(g>>8) + float64(bl>>8)/256 - 32768)
		}
	}
	return grid, nil
}

/* -- Disk cache ------------------------------------------------------- */

// Tiles are immutable, so the cache never needs invalidating — which is what
// makes planning at home and riding with no signal work.

func (s *tileStore) readDisk(key tileKey) ([]byte, bool) {
	if s.cacheRoot == nil {
		return nil, false
	}
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	f, err := s.cacheRoot.Open(key.path())
	if err != nil {
		return nil, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxElevationBodyBytes {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxElevationBodyBytes+1))
	if err != nil || len(raw) > maxElevationBodyBytes {
		return nil, false
	}
	path := key.path()
	entry, indexed := s.diskFiles[path]
	if !indexed {
		s.diskBytes += int64(len(raw))
	} else {
		s.diskBytes += int64(len(raw)) - entry.size
	}
	entry.key = key
	entry.size = int64(len(raw))
	entry.lastAccess = time.Now().UTC()
	s.diskFiles[path] = entry
	return raw, true
}

func (s *tileStore) writeDisk(key tileKey, raw []byte) error {
	if s.cacheRoot == nil {
		return nil
	}
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	path := key.path()
	dir := filepath.Dir(path)
	if err := s.cacheRoot.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = s.cacheRoot.Chmod(dir, 0o700)
	if err := s.makeDiskRoomLocked(path, int64(len(raw))); err != nil {
		return err
	}
	tmp, err := writeTempFileAt(s.cacheRoot, dir, raw)
	if err != nil {
		return err
	}
	defer s.cacheRoot.Remove(tmp)
	// A half-written tile would decode as garbage elevation, so it is only
	// given its real name once complete.
	if err := s.cacheRoot.Rename(tmp, path); err != nil {
		return err
	}
	if err := syncRootDir(s.cacheRoot, dir); err != nil {
		return err
	}
	old := s.diskFiles[path]
	s.diskBytes -= old.size
	s.diskFiles[path] = tileDiskFile{key: key, size: int64(len(raw)), lastAccess: time.Now().UTC()}
	s.diskBytes += int64(len(raw))
	if s.diskBytes > s.maxDiskBytes {
		return errors.New("elevation tile cache quota exceeded after write")
	}
	return nil
}

func (s *tileStore) diskFileSize(key tileKey) (int64, bool) {
	if s.cacheRoot == nil {
		return 0, false
	}
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	entry, ok := s.diskFiles[key.path()]
	if !ok {
		return 0, false
	}
	return entry.size, true
}

func (s *tileStore) touchDisk(key tileKey) {
	if s.cacheRoot == nil {
		return
	}
	s.diskMu.Lock()
	entry, ok := s.diskFiles[key.path()]
	if ok {
		entry.lastAccess = time.Now().UTC()
		s.diskFiles[key.path()] = entry
	}
	s.diskMu.Unlock()
}

func (s *tileStore) loadDiskIndex() error {
	if err := fs.WalkDir(s.cacheRoot.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".png") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		key, ok := tileKeyFromPath(path)
		if !ok {
			return nil
		}
		s.diskFiles[path] = tileDiskFile{key: key, size: info.Size(), lastAccess: info.ModTime()}
		s.diskBytes += info.Size()
		return nil
	}); err != nil {
		return err
	}
	return s.enforceDiskQuotaLocked("")
}

func tileKeyFromPath(path string) (tileKey, bool) {
	var key tileKey
	normalized := filepath.ToSlash(path)
	if _, err := fmt.Sscanf(normalized, "%d/%d/%d.png", &key.z, &key.x, &key.y); err != nil ||
		key.z < 0 || key.x < 0 || key.y < 0 || normalized != key.path() {
		return tileKey{}, false
	}
	return key, true
}

func (s *tileStore) makeDiskRoomLocked(path string, size int64) error {
	if size > s.maxDiskBytes {
		return errors.New("elevation tile exceeds cache quota")
	}
	old := s.diskFiles[path]
	for s.diskBytes-old.size+size > s.maxDiskBytes {
		if err := s.evictOldestDiskLocked(path); err != nil {
			return err
		}
	}
	return nil
}

func (s *tileStore) enforceDiskQuotaLocked(exclude string) error {
	for s.diskBytes > s.maxDiskBytes {
		if err := s.evictOldestDiskLocked(exclude); err != nil {
			return err
		}
	}
	return nil
}

func (s *tileStore) evictOldestDiskLocked(exclude string) error {
	oldestPath := ""
	var oldest tileDiskFile
	for path, entry := range s.diskFiles {
		if path == exclude || (oldestPath != "" && !entry.lastAccess.Before(oldest.lastAccess)) {
			continue
		}
		oldestPath, oldest = path, entry
	}
	if oldestPath == "" {
		return errors.New("elevation tile cache quota cannot be satisfied")
	}
	if err := s.cacheRoot.Remove(oldestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("evict elevation tile: %w", err)
	}
	if err := syncRootDir(s.cacheRoot, filepath.Dir(oldestPath)); err != nil {
		return fmt.Errorf("sync elevation tile eviction: %w", err)
	}
	delete(s.diskFiles, oldestPath)
	s.diskBytes -= oldest.size
	s.mu.Lock()
	if element := s.index[oldest.key]; element != nil {
		s.lru.Remove(element)
		delete(s.index, oldest.key)
	}
	s.mu.Unlock()
	return nil
}

/* -- Prefetch --------------------------------------------------------- */

/*
 * Pulling tiles for the area being planned, ahead of needing them, so the
 * elevation of a freshly routed leg is already local.
 *
 * The cap matters. Tiles are fixed-size squares of ground, so the count grows
 * with the square of how far you zoom out: a viewport around one town is a
 * few dozen, the same window zoomed out to the next province is thousands.
 * Past the cap the prefetch is skipped rather than silently pulling hundreds
 * of megabytes — on-demand lookup still covers whatever the route touches.
 */
const maxPrefetchTiles = 256

type prefetchProgress struct {
	Running bool `json:"running"`
	Done    int  `json:"done"`
	Total   int  `json:"total"`
	// Skipped reports the last request that was refused for being too wide,
	// so the UI can say why nothing is happening.
	Skipped bool `json:"skipped,omitempty"`
	// Clamped reports that the view was wider than the cap and only its
	// middle is being cached, so the UI can say so rather than imply the
	// whole screen is covered.
	Clamped bool   `json:"clamped,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type prefetchState struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	done    int
	total   int
	skipped bool
	clamped bool
	reason  string
}

// tileRange returns the inclusive tile bounds covering a bounding box.
func (s *tileStore) tileRange(south, west, north, east float64) (x0, y0, x1, y1 int) {
	// North is a smaller pixel y than south, hence the corner pairing.
	topX, topY := s.project(point{lat: north, lon: west})
	botX, botY := s.project(point{lat: south, lon: east})
	x0, y0 = int(topX)/tileSize, int(topY)/tileSize
	x1, y1 = int(botX)/tileSize, int(botY)/tileSize
	if x1 < x0 {
		x0, x1 = x1, x0
	}
	if y1 < y0 {
		y0, y1 = y1, y0
	}
	return
}

// have reports whether a tile is already local, in memory or on disk.
func (s *tileStore) have(key tileKey) bool {
	s.mu.Lock()
	_, inMemory := s.index[key]
	s.mu.Unlock()
	if inMemory {
		return true
	}
	_, onDisk := s.readDisk(key)
	return onDisk
}

// startPrefetch pulls every tile covering the box in the background. A newer
// request replaces an older one: the map has moved, and the tiles for where
// it was are no longer the ones wanted.
func (s *tileStore) startPrefetch(south, west, north, east float64) prefetchProgress {
	x0, y0, x1, y1 := s.tileRange(south, west, north, east)
	world := int(math.Exp2(float64(s.zoom)))
	count := (x1 - x0 + 1) * (y1 - y0 + 1)

	s.prefetch.mu.Lock()
	defer s.prefetch.mu.Unlock()

	if s.prefetch.cancel != nil {
		s.prefetch.cancel()
		s.prefetch.cancel = nil
	}

	// Zoomed out, the view runs to thousands of tiles — the planner opens at a
	// whole-province zoom, where refusing outright would mean it never caches
	// anything at all. Cache the middle of the view instead, which is where
	// the work starts, and say that is what happened.
	clamped, reason := false, ""
	if count > maxPrefetchTiles {
		r := (int(math.Sqrt(float64(maxPrefetchTiles))) - 1) / 2
		cx, cy := (x0+x1)/2, (y0+y1)/2
		x0, x1 = max(x0, cx-r), min(x1, cx+r)
		y0, y1 = max(y0, cy-r), min(y1, cy+r)
		clamped = true
		reason = fmt.Sprintf("view spans %d tiles — caching the middle %d",
			count, (x1-x0+1)*(y1-y0+1))
	}

	var missing []tileKey
	for x := x0; x <= x1; x++ {
		for y := y0; y <= y1; y++ {
			if y < 0 || y >= world {
				continue
			}
			key := tileKey{z: s.zoom, x: ((x % world) + world) % world, y: y}
			if !s.have(key) {
				missing = append(missing, key)
			}
		}
	}

	s.prefetch.skipped = false
	s.prefetch.clamped, s.prefetch.reason = clamped, reason
	s.prefetch.done, s.prefetch.total = 0, len(missing)
	if len(missing) == 0 {
		s.prefetch.running = false
		return prefetchProgress{Running: false, Clamped: clamped, Reason: reason}
	}
	if s.offline {
		s.prefetch.running = false
		s.prefetch.skipped = true
		s.prefetch.reason = "cache-only mode: uncached tiles were not fetched"
		return prefetchProgress{Running: false, Total: len(missing), Skipped: true, Reason: s.prefetch.reason}
	}

	ctx, cancel := context.WithCancel(s.ctx)
	s.prefetch.cancel = cancel
	s.prefetch.running = true

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var wg sync.WaitGroup
		for _, key := range missing {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(k tileKey) {
				defer wg.Done()
				// grid() handles the cache, the in-flight dedup and the
				// concurrency limit, so a prefetch and a live lookup asking
				// for the same tile still only fetch it once.
				s.grid(ctx, k)
				s.prefetch.mu.Lock()
				s.prefetch.done++
				s.prefetch.mu.Unlock()
			}(key)
		}
		wg.Wait()
		s.prefetch.mu.Lock()
		s.prefetch.running = false
		s.prefetch.cancel = nil
		s.prefetch.mu.Unlock()
		cancel()
	}()

	return prefetchProgress{Running: true, Done: 0, Total: len(missing), Clamped: clamped, Reason: reason}
}

func (s *tileStore) diskStats() (int64, int) {
	if s.cacheRoot == nil {
		return 0, 0
	}
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	return s.diskBytes, len(s.diskFiles)
}

func (s *tileStore) diskQuota() int64 { return s.maxDiskBytes }

func (s *tileStore) progress() prefetchProgress {
	s.prefetch.mu.Lock()
	defer s.prefetch.mu.Unlock()
	return prefetchProgress{
		Running: s.prefetch.running,
		Done:    s.prefetch.done,
		Total:   s.prefetch.total,
		Skipped: s.prefetch.skipped,
		Clamped: s.prefetch.clamped,
		Reason:  s.prefetch.reason,
	}
}
