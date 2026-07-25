package engine

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

// defaultCacheTTL is how long immutable loom objects are cached when
// Config.CacheTTL is unset. Agents and prompts are versioned and never mutated,
// so the default is effectively permanent — override via Config.CacheTTL only if
// your cache backend enforces eviction.
const defaultCacheTTL = 24 * time.Hour

// defaultLatestCacheTTL is how long a "latest/active" pointer is cached when
// Config.LatestCacheTTL is unset. Unlike a versioned record, this pointer is
// MUTABLE — Create / SetActive change what it resolves to — so it is evicted on
// every such write. The TTL is only a backstop for an eviction that cannot
// reach a given cache: across multiple engine instances sharing an in-process
// cache, a publish on one instance does not evict another's copy, so this TTL
// bounds that staleness. It is deliberately short.
const defaultLatestCacheTTL = 5 * time.Second

// cacheKeyOwnedLatest formats the key for an owner-scoped "latest/active"
// pointer — a resolution with no fixed version. It carries no version component
// (that is the whole point: the pointer moves), but keeps owner for the same
// tenant-isolation reason cacheKeyOwned documents.
func cacheKeyOwnedLatest(kind, prefix, owner, slug string) string {
	return fmt.Sprintf("loom:%s:%s:%s:%s", kind, prefix, owner, slug)
}

// cacheKey formats a cache key for a given kind, prefix, slug, and version. Use
// it only for globally unique addresses (a row UUID); anything addressed by slug
// is owner-scoped and must use cacheKeyOwned.
func cacheKey(kind, prefix, slug string, version int) string {
	return fmt.Sprintf("loom:%s:%s:%s:%d", kind, prefix, slug, version)
}

// cacheKeyOwned formats a cache key for a record addressed by owner+slug+version.
// The owner MUST be part of the key: slugs are only unique within an owner, so a
// key without it would serve one tenant's agent/prompt to another — the cache
// silently defeating the SQL owner filter. "" (the global scope) is just another
// owner value here.
func cacheKeyOwned(kind, prefix, owner, slug string, version int) string {
	return fmt.Sprintf("loom:%s:%s:%s:%s:%d", kind, prefix, owner, slug, version)
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

// cacheSet serialises v and writes it to the cache with the given ttl. Errors
// are silently ignored — a failed cache write degrades to a DB read, not an
// application error.
func cacheSet[T any](ctx context.Context, c Cache, key string, v *T, ttl time.Duration) {
	if c == nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.Set(ctx, key, raw, ttl)
}

// cacheDelete evicts a key. No-op when the cache is nil.
func cacheDelete(ctx context.Context, c Cache, key string) {
	if c == nil {
		return
	}
	c.Delete(ctx, key)
}

// cachedLatest is the read-through wrapper for a mutable latest/active pointer:
// serve from cache on a hit, otherwise resolve, store under the short latest
// TTL, and return. It is bypassed entirely — straight to resolve — when the
// cache is nil or e.latestCacheTTL is negative (latest caching disabled), so a
// caller that wants no pointer caching pays nothing for it.
//
// A resolve error is never cached: a transient miss must not pin a not-found.
func cachedLatest[T any](ctx context.Context, e *Engine, key string, resolve func() (*T, error)) (*T, error) {
	if e.cache == nil || e.latestCacheTTL < 0 {
		return resolve()
	}
	if v, ok := cacheGet[T](ctx, e.cache, key); ok {
		return v, nil
	}
	v, err := resolve()
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, e.cache, key, v, e.latestCacheTTL)
	return v, nil
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
