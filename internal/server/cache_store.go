package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	cacheSchemaVersion = 1
	maxMetadataBytes   = 64 << 10
)

// cacheMetadata is deliberately self-contained. The index can be rebuilt from
// sidecars after an unclean shutdown without exposing request data in paths.
type cacheMetadata struct {
	Schema            int               `json:"schema"`
	Key               string            `json:"key"`
	Scope             string            `json:"scope"`
	Provider          string            `json:"provider"`
	SourceFingerprint string            `json:"sourceFingerprint"`
	Status            int               `json:"status"`
	Headers           map[string]string `json:"headers,omitempty"`
	ContentType       string            `json:"contentType"`
	ContentEncoding   string            `json:"contentEncoding,omitempty"`
	Length            int64             `json:"length"`
	Checksum          string            `json:"checksum"`
	ETag              string            `json:"etag,omitempty"`
	LastModified      string            `json:"lastModified,omitempty"`
	FetchedAt         time.Time         `json:"fetchedAt"`
	FreshUntil        time.Time         `json:"freshUntil"`
	StaleUntil        time.Time         `json:"staleUntil,omitempty"`
	DeleteNoLaterThan time.Time         `json:"deleteNoLaterThan,omitempty"`
	LastAccess        time.Time         `json:"lastAccess"`
	Pins              []string          `json:"pins,omitempty"`
	metadataLength    int64
}

type cacheEntry struct {
	Meta cacheMetadata
	Body []byte
}

type cacheStore struct {
	dir        string
	entriesDir string
	packsDir   string
	tmpDir     string
	entriesRel string
	packsRel   string
	tmpRel     string
	root       *os.Root
	removeFile func(string) error
	maxBytes   int64
	maxEntries int
	reserved   int64
	writable   bool

	mu              sync.Mutex
	writeMu         sync.Mutex
	entries         map[string]*cacheMetadata
	bytes           int64
	now             func() time.Time
	admissionWindow time.Time
	admissions      int
	getHits         atomic.Uint64
	getMisses       atomic.Uint64
}

func (s *cacheStore) setReservedBytes(bytes int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writable {
		s.reserved = 0
		return nil
	}
	if bytes < 0 || bytes > s.maxBytes {
		return fmt.Errorf("offline cache quota is smaller than required control storage")
	}
	s.reserved = bytes
	return nil
}

func newCacheStore(dir string, maxBytes int64, maxEntries int) (*cacheStore, error) {
	return openCacheStore(dir, maxBytes, maxEntries, true)
}

func openCacheStore(dir string, maxBytes int64, maxEntries int, enforceLimits bool) (*cacheStore, error) {
	if maxBytes <= 0 {
		return nil, errors.New("offline cache max bytes must be positive")
	}
	if maxEntries <= 0 {
		return nil, errors.New("offline cache max entries must be positive")
	}
	s := &cacheStore{
		dir:        strings.TrimSpace(dir),
		maxBytes:   maxBytes,
		maxEntries: maxEntries,
		entries:    make(map[string]*cacheMetadata),
		now:        time.Now,
	}
	if s.dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create offline cache: %w", err)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("open offline cache: %w", err)
	}
	s.root = root
	s.removeFile = root.Remove
	if err := s.root.Chmod(".", 0o700); err != nil {
		s.root.Close()
		return nil, fmt.Errorf("secure offline cache: %w", err)
	}
	s.entriesRel = filepath.Join("v1", "entries")
	s.packsRel = filepath.Join("v1", "packs")
	s.tmpRel = filepath.Join("v1", "tmp")
	s.entriesDir = filepath.Join(s.dir, s.entriesRel)
	s.packsDir = filepath.Join(s.dir, s.packsRel)
	s.tmpDir = filepath.Join(s.dir, s.tmpRel)
	for _, path := range []string{s.entriesRel, s.packsRel, s.tmpRel} {
		if err := s.root.MkdirAll(path, 0o700); err != nil {
			s.root.Close()
			return nil, fmt.Errorf("create offline cache: %w", err)
		}
		if err := s.root.Chmod(path, 0o700); err != nil {
			s.root.Close()
			return nil, fmt.Errorf("secure offline cache: %w", err)
		}
	}
	s.writable = true
	if err := s.load(); err != nil {
		s.root.Close()
		return nil, err
	}
	if enforceLimits {
		if err := s.enforceLoadedLimits(); err != nil {
			s.root.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *cacheStore) close() error {
	if s.root == nil {
		return nil
	}
	return s.root.Close()
}

func canonicalCacheKey(provider, source, method, params, language string, body []byte) string {
	h := sha256.New()
	for _, value := range []string{
		fmt.Sprint(cacheSchemaVersion), provider, source, strings.ToUpper(method), params, language,
	} {
		io.WriteString(h, fmt.Sprintf("%d:", len(value)))
		io.WriteString(h, value)
	}
	fmt.Fprintf(h, "%d:", len(body))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func validScope(scope string) bool {
	if scope == "" || len(scope) > 48 {
		return false
	}
	for _, r := range scope {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func validCacheKey(key string) bool {
	if len(key) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(key)
	return err == nil
}

func (s *cacheStore) paths(scope, key string) (string, string, error) {
	body, meta, err := s.relativePaths(scope, key)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(s.dir, body), filepath.Join(s.dir, meta), nil
}

func (s *cacheStore) relativePaths(scope, key string) (string, string, error) {
	if !validScope(scope) || !validCacheKey(key) {
		return "", "", errors.New("invalid cache identity")
	}
	dir := filepath.Join(s.entriesRel, scope, key[:2])
	return filepath.Join(dir, key+".body"), filepath.Join(dir, key+".json"), nil
}

func (s *cacheStore) load() error {
	s.recoverBackups()
	if err := fs.WalkDir(s.root.FS(), filepath.ToSlash(s.entriesRel), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.Contains(d.Name(), ".tmp-") {
			_ = s.root.Remove(path)
			return nil
		}
		if strings.HasSuffix(d.Name(), ".body") {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			_ = s.root.Remove(path)
			return nil
		}
		raw, err := readFileLimitAt(s.root, path, maxMetadataBytes)
		if err != nil {
			_ = s.root.Remove(path)
			return nil
		}
		var meta cacheMetadata
		if json.Unmarshal(raw, &meta) != nil || meta.Schema != cacheSchemaVersion ||
			!validScope(meta.Scope) || !validCacheKey(meta.Key) || meta.Length < 0 {
			s.removePairByMetadataPath(path)
			return nil
		}
		bodyPath, metaPath, err := s.relativePaths(meta.Scope, meta.Key)
		if err != nil || filepath.Clean(metaPath) != filepath.Clean(path) {
			s.removePairByMetadataPath(path)
			return nil
		}
		body, err := readFileLimitAt(s.root, bodyPath, meta.Length)
		if err != nil || int64(len(body)) != meta.Length || checksum(body) != meta.Checksum {
			_ = s.root.Remove(bodyPath)
			_ = s.root.Remove(metaPath)
			return nil
		}
		meta.metadataLength = int64(len(raw))
		if old := s.entries[meta.Key]; old != nil {
			s.bytes -= old.Length + old.metadataLength
		}
		copyMeta := meta
		s.entries[meta.Key] = &copyMeta
		s.bytes += meta.Length + meta.metadataLength
		return nil
	}); err != nil {
		return err
	}
	// Metadata is authoritative. Bodies left without a valid sidecar are
	// interrupted writes or corruption and must not consume quota silently.
	_ = fs.WalkDir(s.root.FS(), filepath.ToSlash(s.entriesRel), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".body") {
			return nil
		}
		key := strings.TrimSuffix(d.Name(), ".body")
		s.mu.Lock()
		_, indexed := s.entries[key]
		s.mu.Unlock()
		if !indexed {
			_ = s.root.Remove(path)
		}
		return nil
	})
	entries, _ := fs.ReadDir(s.root.FS(), filepath.ToSlash(s.tmpRel))
	for _, entry := range entries {
		if !entry.IsDir() {
			_ = s.root.Remove(filepath.Join(s.tmpRel, entry.Name()))
		}
	}
	return nil
}

func (s *cacheStore) recoverBackups() {
	_ = fs.WalkDir(s.root.FS(), filepath.ToSlash(s.entriesRel), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".body.bak") {
			return nil
		}
		metaBackup := strings.TrimSuffix(path, ".body.bak") + ".json.bak"
		if _, err := s.root.Lstat(metaBackup); errors.Is(err, os.ErrNotExist) {
			bodyPath := strings.TrimSuffix(path, ".bak")
			_ = s.root.Remove(bodyPath)
			_ = s.root.Rename(path, bodyPath)
		}
		return nil
	})
	_ = fs.WalkDir(s.root.FS(), filepath.ToSlash(s.entriesRel), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json.bak") {
			return nil
		}
		raw, readErr := readFileLimitAt(s.root, path, maxMetadataBytes)
		var backup cacheMetadata
		if readErr != nil || json.Unmarshal(raw, &backup) != nil || !validScope(backup.Scope) || !validCacheKey(backup.Key) {
			_ = s.root.Remove(path)
			_ = s.root.Remove(strings.TrimSuffix(path, ".json.bak") + ".body.bak")
			return nil
		}
		bodyPath, metaPath, pathsErr := s.relativePaths(backup.Scope, backup.Key)
		bodyBackup := bodyPath + ".bak"
		if pathsErr != nil || !validCachePairAt(s.root, metaPath, bodyPath) {
			_ = s.root.Remove(bodyPath)
			_ = s.root.Remove(metaPath)
			_ = s.root.Rename(bodyBackup, bodyPath)
			_ = s.root.Rename(path, metaPath)
		} else {
			_ = s.root.Remove(bodyBackup)
			_ = s.root.Remove(path)
		}
		return nil
	})
}

func validCachePairAt(root *os.Root, metaPath, bodyPath string) bool {
	raw, err := readFileLimitAt(root, metaPath, maxMetadataBytes)
	if err != nil {
		return false
	}
	var meta cacheMetadata
	if json.Unmarshal(raw, &meta) != nil || meta.Length < 0 {
		return false
	}
	body, err := readFileLimitAt(root, bodyPath, meta.Length)
	return err == nil && int64(len(body)) == meta.Length && checksum(body) == meta.Checksum
}

func readFileLimitAt(root *os.Root, path string, max int64) ([]byte, error) {
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("cache file is not regular")
		}
		return nil, err
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, errors.New("file exceeds declared limit")
	}
	return raw, nil
}

func (s *cacheStore) enforceLoadedLimits() error {
	if !s.writable {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.removeExpiredLoadedEntriesLocked(s.now().UTC()); err != nil {
		return err
	}
	counts, candidates := s.loadedEvictionStateLocked()
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].LastAccess.Equal(candidates[j].LastAccess) {
			return candidates[i].Key < candidates[j].Key
		}
		return candidates[i].LastAccess.Before(candidates[j].LastAccess)
	})
	if err := s.evictLoadedGlobalLocked(candidates, counts); err != nil {
		return err
	}
	return s.evictLoadedScopesLocked(candidates, counts)
}

func (s *cacheStore) removeExpiredLoadedEntriesLocked(now time.Time) error {
	for key, meta := range s.entries {
		hardExpired := !meta.DeleteNoLaterThan.IsZero() && !now.Before(meta.DeleteNoLaterThan)
		staleExpired := len(meta.Pins) == 0 && !meta.StaleUntil.IsZero() && !now.Before(meta.StaleUntil)
		if hardExpired || staleExpired {
			if _, err := s.removeLocked(meta.Scope, key); err != nil {
				return fmt.Errorf("remove expired cache entry: %w", err)
			}
		}
	}
	return nil
}

func (s *cacheStore) loadedEvictionStateLocked() (map[string]int, []*cacheMetadata) {
	counts := make(map[string]int)
	candidates := make([]*cacheMetadata, 0, len(s.entries))
	for _, meta := range s.entries {
		counts[meta.Scope]++
		if len(meta.Pins) == 0 {
			candidates = append(candidates, meta)
		}
	}
	return counts, candidates
}

func (s *cacheStore) evictLoadedGlobalLocked(candidates []*cacheMetadata, counts map[string]int) error {
	for _, candidate := range candidates {
		if len(s.entries) <= s.maxEntries && s.bytes <= s.maxBytes-s.reserved {
			break
		}
		if err := s.evictLoadedCandidateLocked(candidate, counts); err != nil {
			return err
		}
	}
	return nil
}

func (s *cacheStore) evictLoadedScopesLocked(candidates []*cacheMetadata, counts map[string]int) error {
	perScopeCap := max(1, s.maxEntries/4)
	for _, candidate := range candidates {
		if counts[candidate.Scope] <= perScopeCap {
			continue
		}
		if err := s.evictLoadedCandidateLocked(candidate, counts); err != nil {
			return err
		}
	}
	return nil
}

func (s *cacheStore) evictLoadedCandidateLocked(candidate *cacheMetadata, counts map[string]int) error {
	current := s.entries[candidate.Key]
	if current == nil || len(current.Pins) != 0 {
		return nil
	}
	removed, err := s.removeLocked(current.Scope, current.Key)
	if removed {
		counts[current.Scope]--
	}
	return err
}

// reconcilePins replaces authoritative pin state without exposing an
// intermediate unpinned cache. Limit enforcement runs after reconciliation.
func (s *cacheStore) reconcilePins(desired map[string][]string) (map[string]struct{}, int, error) {
	unavailable := make(map[string]struct{})
	if !s.writable {
		return unavailable, 0, nil
	}

	normalized := make(map[string][]string, len(desired))
	for key, pins := range desired {
		if !validCacheKey(key) {
			unavailable[key] = struct{}{}
			continue
		}
		pins = normalizePins(pins)
		if len(pins) != 0 {
			normalized[key] = pins
		}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	for key := range normalized {
		meta := s.entries[key]
		if meta == nil || !staleAllowed(*meta, now) {
			unavailable[key] = struct{}{}
			delete(normalized, key)
		}
	}

	keys := make([]string, 0, len(s.entries))
	for key := range s.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changed := 0
	for _, key := range keys {
		current := s.entries[key]
		pins := normalized[key]
		updated, err := s.updatePinsLocked(current, pins)
		if err != nil {
			return unavailable, changed, err
		}
		if updated {
			changed++
		}
	}
	return unavailable, changed, nil
}

func (s *cacheStore) reconcilePinOwner(owner string, desiredKeys []string) (map[string]struct{}, int, error) {
	return s.updatePinOwner(owner, desiredKeys, true)
}

func (s *cacheStore) addPinOwner(owner string, desiredKeys []string) (map[string]struct{}, int, error) {
	return s.updatePinOwner(owner, desiredKeys, false)
}

func (s *cacheStore) updatePinOwner(owner string, desiredKeys []string, replace bool) (map[string]struct{}, int, error) {
	unavailable := make(map[string]struct{})
	if !s.writable {
		return unavailable, 0, nil
	}
	if owner == "" {
		return unavailable, 0, errors.New("cache pin owner is required")
	}
	desired := make(map[string]struct{}, len(desiredKeys))
	for _, key := range desiredKeys {
		if validCacheKey(key) {
			desired[key] = struct{}{}
		} else {
			unavailable[key] = struct{}{}
		}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	for key := range desired {
		meta := s.entries[key]
		if meta == nil || !staleAllowed(*meta, now) {
			unavailable[key] = struct{}{}
			delete(desired, key)
		}
	}
	keys := make([]string, 0, len(s.entries))
	for key := range s.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changed := 0
	for _, key := range keys {
		current := s.entries[key]
		pins := make([]string, 0, len(current.Pins)+1)
		_, keep := desired[key]
		for _, pin := range current.Pins {
			if pin != owner || !replace || keep {
				pins = append(pins, pin)
			}
		}
		if keep {
			pins = append(pins, owner)
		}
		updated, err := s.updatePinsLocked(current, normalizePins(pins))
		if err != nil {
			return unavailable, changed, err
		}
		if updated {
			changed++
		}
	}
	return unavailable, changed, nil
}

func normalizePins(pins []string) []string {
	pins = append([]string(nil), pins...)
	sort.Strings(pins)
	unique := pins[:0]
	for _, pin := range pins {
		if pin != "" && (len(unique) == 0 || unique[len(unique)-1] != pin) {
			unique = append(unique, pin)
		}
	}
	return unique
}

func (s *cacheStore) updatePinsLocked(current *cacheMetadata, pins []string) (bool, error) {
	if slices.Equal(current.Pins, pins) {
		return false, nil
	}
	copyMeta := *current
	copyMeta.Pins = append([]string(nil), pins...)
	raw, err := json.Marshal(copyMeta)
	if err != nil || len(raw) > maxMetadataBytes {
		return false, errors.New("invalid cache pin metadata")
	}
	_, metaPath, err := s.relativePaths(copyMeta.Scope, copyMeta.Key)
	if err != nil {
		return false, err
	}
	if err := atomicWriteFileAt(s.root, s.tmpRel, metaPath, raw); err != nil {
		return false, err
	}
	s.bytes -= current.metadataLength
	copyMeta.metadataLength = int64(len(raw))
	s.entries[current.Key] = &copyMeta
	s.bytes += copyMeta.metadataLength
	return true, nil
}

func (s *cacheStore) removePairByMetadataPath(metaPath string) {
	_ = s.root.Remove(metaPath)
	_ = s.root.Remove(strings.TrimSuffix(metaPath, ".json") + ".body")
}

func checksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (s *cacheStore) get(scope, key string) (*cacheEntry, bool) {
	if !s.writable {
		s.getMisses.Add(1)
		return nil, false
	}
	s.mu.Lock()
	meta, ok := s.entries[key]
	if !ok || meta.Scope != scope {
		s.mu.Unlock()
		s.getMisses.Add(1)
		return nil, false
	}
	copyMeta := *meta
	copyMeta.Headers = cloneStringMap(meta.Headers)
	copyMeta.Pins = append([]string(nil), meta.Pins...)
	s.mu.Unlock()

	bodyPath, _, err := s.relativePaths(scope, key)
	if err != nil {
		s.getMisses.Add(1)
		return nil, false
	}
	body, err := readFileLimitAt(s.root, bodyPath, copyMeta.Length)
	if err != nil || int64(len(body)) != copyMeta.Length || checksum(body) != copyMeta.Checksum {
		_ = s.remove(scope, key)
		s.getMisses.Add(1)
		return nil, false
	}

	s.mu.Lock()
	if current := s.entries[key]; current != nil {
		current.LastAccess = s.now().UTC()
	}
	s.mu.Unlock()
	s.getHits.Add(1)
	return &cacheEntry{Meta: copyMeta, Body: body}, true
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (s *cacheStore) put(meta cacheMetadata, body []byte) error {
	_, err := s.putWithAdmission(meta, body, nil)
	return err
}

func (s *cacheStore) putWithAdmission(meta cacheMetadata, body []byte, admit func(int64) error) (int64, error) {
	if !s.writable {
		return 0, nil
	}
	if !validScope(meta.Scope) || !validCacheKey(meta.Key) {
		return 0, errors.New("invalid cache identity")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	meta.Schema = cacheSchemaVersion
	meta.Length = int64(len(body))
	meta.Checksum = checksum(body)
	if meta.LastAccess.IsZero() {
		meta.LastAccess = meta.FetchedAt
	}
	s.mu.Lock()
	old := s.entries[meta.Key]
	replacing := old != nil
	if old != nil {
		meta.Pins = append([]string(nil), old.Pins...)
	}
	s.mu.Unlock()
	raw, err := json.Marshal(meta)
	if err != nil {
		return 0, err
	}
	if len(raw) > maxMetadataBytes {
		return 0, errors.New("cache metadata too large")
	}
	meta.metadataLength = int64(len(raw))
	required := meta.Length + meta.metadataLength
	if required > s.maxBytes {
		return 0, errors.New("cache entry exceeds quota")
	}
	oldBytes := int64(0)
	if old != nil {
		oldBytes = old.Length + old.metadataLength
	}
	admitted := max(int64(0), required-oldBytes)
	if admit != nil {
		if err := admit(admitted); err != nil {
			return 0, err
		}
	}

	s.mu.Lock()
	if s.entries[meta.Key] == nil {
		now := s.now()
		if s.admissionWindow.IsZero() || now.Sub(s.admissionWindow) >= time.Minute {
			s.admissionWindow, s.admissions = now, 0
		}
		limit := max(64, min(1000, s.maxEntries/100))
		if s.admissions >= limit {
			s.mu.Unlock()
			return 0, errors.New("cache distinct-key admission rate exceeded")
		}
		s.admissions++
	}
	if err := s.admitLocked(meta.Scope, meta.Key, required); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	s.mu.Unlock()

	bodyPath, metaPath, _ := s.relativePaths(meta.Scope, meta.Key)
	if err := s.root.MkdirAll(filepath.Dir(bodyPath), 0o700); err != nil {
		return 0, err
	}
	_ = s.root.Chmod(filepath.Dir(bodyPath), 0o700)
	if err := replaceCachePairAt(s.root, s.tmpRel, bodyPath, metaPath, body, raw, replacing); err != nil {
		return 0, err
	}

	s.mu.Lock()
	if old := s.entries[meta.Key]; old != nil {
		s.bytes -= old.Length + old.metadataLength
	}
	copyMeta := meta
	s.entries[meta.Key] = &copyMeta
	s.bytes += required
	s.mu.Unlock()
	return admitted, nil
}

func atomicWriteFileAt(root *os.Root, tmpDir, destination string, data []byte) error {
	name, err := writeTempFileAt(root, tmpDir, data)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	if err := root.Rename(name, destination); err != nil {
		return err
	}
	if err := syncRootDir(root, filepath.Dir(destination)); err != nil {
		return err
	}
	return syncRootDir(root, tmpDir)
}

func writeTempFileAt(root *os.Root, tmpDir string, data []byte) (string, error) {
	var tmp *os.File
	var name string
	for range 100 {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", err
		}
		name = filepath.Join(tmpDir, ".tmp-"+hex.EncodeToString(suffix[:]))
		var err error
		tmp, err = root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	if tmp == nil {
		return "", errors.New("could not allocate a temporary cache file")
	}
	failed := true
	defer func() {
		_ = tmp.Close()
		if failed {
			_ = root.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	failed = false
	return name, nil
}

func replaceCachePairAt(root *os.Root, tmpDir, bodyPath, metaPath string, body, meta []byte, replacing bool) error {
	bodyTemp, err := writeTempFileAt(root, tmpDir, body)
	if err != nil {
		return err
	}
	defer root.Remove(bodyTemp)
	metaTemp, err := writeTempFileAt(root, tmpDir, meta)
	if err != nil {
		return err
	}
	defer root.Remove(metaTemp)
	bodyBackup, metaBackup := bodyPath+".bak", metaPath+".bak"
	if replacing {
		_ = root.Remove(bodyBackup)
		_ = root.Remove(metaBackup)
		if err := root.Rename(bodyPath, bodyBackup); err != nil {
			return err
		}
		if err := root.Rename(metaPath, metaBackup); err != nil {
			_ = root.Rename(bodyBackup, bodyPath)
			return err
		}
	}
	rollback := func() {
		_ = root.Remove(bodyPath)
		_ = root.Remove(metaPath)
		if replacing {
			_ = root.Rename(bodyBackup, bodyPath)
			_ = root.Rename(metaBackup, metaPath)
		}
	}
	if err := root.Rename(bodyTemp, bodyPath); err != nil {
		rollback()
		return err
	}
	if err := root.Rename(metaTemp, metaPath); err != nil {
		rollback()
		return err
	}
	_ = root.Remove(bodyBackup)
	_ = root.Remove(metaBackup)
	if err := syncRootDir(root, filepath.Dir(bodyPath)); err != nil {
		return err
	}
	return syncRootDir(root, tmpDir)
}

func syncRootDir(root *os.Root, path string) error {
	dir, err := root.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *cacheStore) admitLocked(scope, replacing string, required int64) error {
	now := s.now()
	for key, meta := range s.entries {
		if key == replacing {
			continue
		}
		if !meta.DeleteNoLaterThan.IsZero() && !now.Before(meta.DeleteNoLaterThan) {
			if _, err := s.removeLocked(meta.Scope, key); err != nil {
				return fmt.Errorf("remove expired cache entry: %w", err)
			}
			continue
		}
		if len(meta.Pins) == 0 && !meta.StaleUntil.IsZero() && !now.Before(meta.StaleUntil) {
			if _, err := s.removeLocked(meta.Scope, key); err != nil {
				return fmt.Errorf("remove stale cache entry: %w", err)
			}
		}
	}

	oldBytes := int64(0)
	oldCount := 0
	if old := s.entries[replacing]; old != nil {
		oldBytes = old.Length + old.metadataLength
		oldCount = 1
	}
	perScopeCap := max(1, s.maxEntries/4)
	for len(s.entries)-oldCount >= s.maxEntries || s.scopeCountLocked(scope)-oldCount >= perScopeCap ||
		s.bytes-oldBytes+required > s.maxBytes-s.reserved {
		evicted, err := s.evictLRULocked(replacing)
		if err != nil {
			return fmt.Errorf("evict cache entry: %w", err)
		}
		if !evicted {
			break
		}
	}
	if len(s.entries)-oldCount >= s.maxEntries || s.scopeCountLocked(scope)-oldCount >= perScopeCap {
		return errors.New("cache entry limit reached by pinned data")
	}
	if s.bytes-oldBytes+required > s.maxBytes-s.reserved {
		return errors.New("cache byte quota reached by pinned data")
	}
	return nil
}

func (s *cacheStore) scopeCountLocked(scope string) int {
	count := 0
	for _, meta := range s.entries {
		if meta.Scope == scope {
			count++
		}
	}
	return count
}

func (s *cacheStore) evictLRULocked(exclude string) (bool, error) {
	var candidate *cacheMetadata
	for key, meta := range s.entries {
		if key == exclude || len(meta.Pins) != 0 {
			continue
		}
		if candidate == nil || meta.LastAccess.Before(candidate.LastAccess) {
			candidate = meta
		}
	}
	if candidate == nil {
		return false, nil
	}
	return s.removeLocked(candidate.Scope, candidate.Key)
}

func (s *cacheStore) remove(scope, key string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.removeLocked(scope, key)
	return err
}

func (s *cacheStore) removeLocked(scope, key string) (bool, error) {
	meta := s.entries[key]
	if meta == nil || meta.Scope != scope {
		return false, nil
	}
	bodyPath, metaPath, err := s.relativePaths(scope, key)
	if err != nil {
		return false, err
	}
	remove := s.removeFile
	if remove == nil {
		remove = s.root.Remove
	}
	if err := remove(bodyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("remove cache body: %w", err)
	}
	if err := remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("remove cache metadata: %w", err)
	}
	if err := syncRootDir(s.root, filepath.Dir(bodyPath)); err != nil {
		return false, fmt.Errorf("sync cache deletion: %w", err)
	}
	s.bytes -= meta.Length + meta.metadataLength
	delete(s.entries, key)
	return true, nil
}

func (s *cacheStore) updateMetadata(meta cacheMetadata) error {
	if !s.writable {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.updateMetadataLocked(meta)
}

func (s *cacheStore) updateMetadataLocked(meta cacheMetadata) error {
	raw, err := json.Marshal(meta)
	if err != nil || len(raw) > maxMetadataBytes {
		return errors.New("invalid cache metadata update")
	}
	_, metaPath, err := s.relativePaths(meta.Scope, meta.Key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	current := s.entries[meta.Key]
	if current == nil {
		s.mu.Unlock()
		return nil
	}
	if err := s.admitLocked(meta.Scope, meta.Key, meta.Length+int64(len(raw))); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	if err := atomicWriteFileAt(s.root, s.tmpRel, metaPath, raw); err != nil {
		return err
	}
	s.mu.Lock()
	if old := s.entries[meta.Key]; old != nil {
		s.bytes -= old.metadataLength
		meta.metadataLength = int64(len(raw))
		copyMeta := meta
		s.entries[meta.Key] = &copyMeta
		s.bytes += meta.metadataLength
	}
	s.mu.Unlock()
	return nil
}

func (s *cacheStore) flushAccesses() {
	if !s.writable {
		return
	}
	s.mu.Lock()
	metas := make([]cacheMetadata, 0, len(s.entries))
	for _, meta := range s.entries {
		copyMeta := *meta
		copyMeta.Headers = cloneStringMap(meta.Headers)
		copyMeta.Pins = append([]string(nil), meta.Pins...)
		metas = append(metas, copyMeta)
	}
	s.mu.Unlock()
	for _, meta := range metas {
		_ = s.updateMetadata(meta)
	}
}

func (s *cacheStore) pin(key, packID string, pinned bool) error {
	if !s.writable || !validCacheKey(key) || packID == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	meta := s.entries[key]
	if meta == nil {
		s.mu.Unlock()
		return errors.New("cache entry is not available")
	}
	if pinned && !staleAllowed(*meta, s.now().UTC()) {
		s.mu.Unlock()
		return errors.New("cache entry is no longer retained")
	}
	copyMeta := *meta
	copyMeta.Pins = append([]string(nil), meta.Pins...)
	s.mu.Unlock()
	index := -1
	for i, id := range copyMeta.Pins {
		if id == packID {
			index = i
			break
		}
	}
	if pinned && index < 0 {
		copyMeta.Pins = append(copyMeta.Pins, packID)
		sort.Strings(copyMeta.Pins)
	}
	if !pinned && index >= 0 {
		copyMeta.Pins = append(copyMeta.Pins[:index], copyMeta.Pins[index+1:]...)
	}
	return s.updateMetadataLocked(copyMeta)
}

func (s *cacheStore) clear(scope string) (int, error) {
	if scope != "" && !validScope(scope) {
		return 0, errors.New("invalid cache scope")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for key, meta := range s.entries {
		if (scope == "" || meta.Scope == scope) && len(meta.Pins) == 0 {
			deleted, err := s.removeLocked(meta.Scope, key)
			if err != nil {
				return removed, err
			}
			if deleted {
				removed++
			}
		}
	}
	return removed, nil
}

type cacheStats struct {
	Writable bool                      `json:"writable"`
	Bytes    int64                     `json:"bytes"`
	Quota    int64                     `json:"quota"`
	Reserved int64                     `json:"reservedBytes"`
	Entries  int                       `json:"entries"`
	Hits     uint64                    `json:"hits"`
	Misses   uint64                    `json:"misses"`
	Scopes   map[string]cacheScopeStat `json:"scopes"`
}

type cacheScopeStat struct {
	Bytes   int64 `json:"bytes"`
	Entries int   `json:"entries"`
	Pinned  int   `json:"pinned"`
}

func (s *cacheStore) stats() cacheStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := cacheStats{Writable: s.writable, Bytes: s.bytes, Quota: s.maxBytes, Reserved: s.reserved, Entries: len(s.entries), Hits: s.getHits.Load(), Misses: s.getMisses.Load(), Scopes: map[string]cacheScopeStat{}}
	for _, meta := range s.entries {
		scope := stats.Scopes[meta.Scope]
		scope.Bytes += meta.Length + meta.metadataLength
		scope.Entries++
		if len(meta.Pins) != 0 {
			scope.Pinned++
		}
		stats.Scopes[meta.Scope] = scope
	}
	return stats
}

func (s *cacheStore) keysForScope(scope string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key, meta := range s.entries {
		if meta.Scope == scope {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func (s *cacheStore) retainedKeys(scope string, now time.Time) ([]string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	var bytes int64
	for key, meta := range s.entries {
		if meta.Scope == scope && staleAllowed(*meta, now) {
			keys = append(keys, key)
			bytes += meta.Length + meta.metadataLength
		}
	}
	sort.Strings(keys)
	return keys, bytes
}
