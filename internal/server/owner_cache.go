package server

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultOwnerCacheBytes   = int64(64 << 20)
	defaultOwnerCacheEntries = 4096
	// maxOpenOwnerCaches bounds the indexes held in memory. Beyond it the
	// least recently used owner's store is closed; a request racing that
	// close is served uncached, which is what a miss would have been.
	maxOpenOwnerCaches = 256
)

// ownerCaches hands out each owner's private response cache, opened lazily
// at owners/<owner>/cache/. It holds place searches and exact elevation
// lookups: the requests that describe the person making them. The shared
// cache never sees them, so its headers cannot tell one user what another
// searched for.
type ownerCaches struct {
	spaces     *ownerSpaces
	maxBytes   int64
	maxEntries int
	enabled    bool

	mu   sync.Mutex
	open map[Owner]*openOwnerCache
}

type openOwnerCache struct {
	store   *cacheStore
	lastUse time.Time
}

func newOwnerCaches(spaces *ownerSpaces, enabled bool, maxBytes int64, maxEntries int) *ownerCaches {
	if maxBytes <= 0 {
		maxBytes = defaultOwnerCacheBytes
	}
	if maxEntries <= 0 {
		maxEntries = defaultOwnerCacheEntries
	}
	return &ownerCaches{spaces: spaces, maxBytes: maxBytes, maxEntries: maxEntries, enabled: enabled, open: make(map[Owner]*openOwnerCache)}
}

// get returns the owner's store, or nil when persistence is disabled.
func (c *ownerCaches) get(owner Owner) *cacheStore {
	store, err := c.lookup(owner)
	if err != nil {
		return nil
	}
	return store
}

func (c *ownerCaches) lookup(owner Owner) (*cacheStore, error) {
	if c == nil || !c.enabled || !owner.Valid() {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.open[owner]; entry != nil {
		entry.lastUse = time.Now()
		return entry.store, nil
	}
	// Create the owner directory with the server's modes before the store
	// creates its own subdirectories inside it.
	root, err := c.spaces.open(owner)
	if err != nil {
		return nil, err
	}
	root.Close()
	store, err := openCacheStore(filepath.Join(c.spaces.absolute(owner), ownerCacheDir), c.maxBytes, c.maxEntries, true)
	if err != nil {
		return nil, err
	}
	if len(c.open) >= maxOpenOwnerCaches {
		c.evictLocked()
	}
	c.open[owner] = &openOwnerCache{store: store, lastUse: time.Now()}
	return store, nil
}

func (c *ownerCaches) evictLocked() {
	var oldest Owner
	var oldestUse time.Time
	for owner, entry := range c.open {
		if oldest == "" || entry.lastUse.Before(oldestUse) {
			oldest, oldestUse = owner, entry.lastUse
		}
	}
	if entry := c.open[oldest]; entry != nil {
		entry.store.flushAccesses()
		_ = entry.store.close()
		delete(c.open, oldest)
	}
}

// storeFor is the outbound client's hook: the caller's store from its context.
func (c *ownerCaches) storeFor(ctx context.Context) *cacheStore {
	owner, ok := OwnerFrom(ctx)
	if !ok {
		return nil
	}
	return c.get(owner)
}

// clear removes the owner's unpinned entries in one scope, or all of them.
func (c *ownerCaches) clear(owner Owner, scope string) (int, error) {
	store, err := c.lookup(owner)
	if err != nil {
		return 0, err
	}
	if store == nil {
		return 0, nil
	}
	return store.clear(scope)
}

// stats reports the owner's cache, empty when there is none.
func (c *ownerCaches) stats(owner Owner) cacheStats {
	store := c.get(owner)
	if store == nil {
		return cacheStats{Scopes: map[string]cacheScopeStat{}}
	}
	return store.stats()
}

// ownerCacheStats is the aggregate across every open owner store. It counts
// stores, never owners by name, so telemetry stays free of identities.
type ownerCacheStats struct {
	Open    int
	Entries int
	Bytes   int64
	Hits    uint64
	Misses  uint64
}

func (c *ownerCaches) aggregate() ownerCacheStats {
	var total ownerCacheStats
	if c == nil {
		return total
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.open {
		stats := entry.store.stats()
		total.Open++
		total.Entries += stats.Entries
		total.Bytes += stats.Bytes
		total.Hits += stats.Hits
		total.Misses += stats.Misses
	}
	return total
}

// close flushes and closes every open store.
func (c *ownerCaches) close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	for owner, entry := range c.open {
		entry.store.flushAccesses()
		err = errors.Join(err, entry.store.close())
		delete(c.open, owner)
	}
	return err
}
