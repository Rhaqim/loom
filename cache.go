package loom

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Cache is a read-through cache for immutable engine objects (agents, prompts).
//
// Agents and prompts are addressed by slug+version and never mutated after
// creation, so they are safe to cache indefinitely. The cache sits in front of
// the database on every RunStep hot-path.
//
// Implement this interface with any backend you already use:
//   - In-process: use NewInProcessCache() (built-in, no dependencies)
//   - Redis: wrap go-redis or redigo
//   - Memcached: wrap gomemcache
//   - Ristretto / BigCache: wrap the client
//
// A nil Cache in Config disables caching (the default).
type Cache interface {
	// Get retrieves a cached value. The second return is false on a cache miss.
	Get(ctx context.Context, key string) ([]byte, bool)
	// Set stores a value. Implementations may ignore ttl if not supported.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
	// Delete removes a key. Used when an entry is explicitly evicted.
	Delete(ctx context.Context, key string)
}

// CacheTTL controls how long immutable loom objects are cached.
// Agents and prompts are versioned and never mutated, so the default is
// effectively permanent — adjust only if your cache backend enforces eviction.
var CacheTTL = 24 * time.Hour

// cacheKey formats a cache key for a given kind, prefix, slug, and version.
func cacheKey(kind, prefix, slug string, version int) string {
	return fmt.Sprintf("loom:%s:%s:%s:%d", kind, prefix, slug, version)
}

// cacheGet attempts to deserialise a cached value into dst.
// Returns false on a miss or a JSON error.
func cacheGet[T any](ctx context.Context, c Cache, key string) (*T, bool) {
	if c == nil {
		return nil, false
	}
	raw, ok := c.Get(ctx, key)
	if !ok {
		return nil, false
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return &v, true
}

// cacheSet serialises v and writes it to the cache. Errors are silently ignored
// — a failed cache write degrades to a DB read, not an application error.
func cacheSet[T any](ctx context.Context, c Cache, key string, v *T) {
	if c == nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.Set(ctx, key, raw, CacheTTL)
}

// -----------------------------------------------------------------------
// Built-in in-process cache (sync.Map, no dependencies)
// -----------------------------------------------------------------------

// inProcessEntry is a single cached value with its expiry timestamp.
type inProcessEntry struct {
	value     []byte
	expiresAt time.Time
}

// InProcessCache is a lightweight in-process cache backed by a sync.Map.
// It supports optional TTL-based expiry, checked lazily on Get.
// Suitable for single-instance deployments or integration testing.
type InProcessCache struct {
	m sync.Map
}

// NewInProcessCache returns a ready-to-use InProcessCache.
func NewInProcessCache() *InProcessCache { return &InProcessCache{} }

func (c *InProcessCache) Get(_ context.Context, key string) ([]byte, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return nil, false
	}
	e := v.(inProcessEntry)
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		c.m.Delete(key)
		return nil, false
	}
	return e.value, true
}

func (c *InProcessCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) {
	e := inProcessEntry{value: value}
	if ttl > 0 {
		e.expiresAt = time.Now().Add(ttl)
	}
	c.m.Store(key, e)
}

func (c *InProcessCache) Delete(_ context.Context, key string) {
	c.m.Delete(key)
}
