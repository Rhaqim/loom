package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Internal services
// -----------------------------------------------------------------------

// agentService implements AgentRegistry.
type agentService struct{ e *Engine }

func (s *agentService) Get(ctx context.Context, owner, slug string, version int) (*Agent, error) {
	if version == 0 {
		return s.Latest(ctx, owner, slug)
	}
	// Cache check — agents are immutable after creation. The key carries owner:
	// slugs are unique only within one, so an unowned key would leak a record
	// across tenants.
	key := s.e.cacheKeyOwned("agent", owner, slug, version)
	if a, ok := cacheGet[Agent](ctx, s.e.cache, key); ok {
		return a, nil
	}
	a, err := queryAgent(ctx, s.e.db, s.e.prefix, owner, slug, version)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, a, s.e.cacheTTL)
	return a, nil
}

func (s *agentService) Latest(ctx context.Context, owner, slug string) (*Agent, error) {
	// Latest is a mutable pointer (Create moves it), so it uses the short latest
	// TTL and is evicted on Create — unlike the version-pinned Get above, which
	// caches immutably.
	key := s.e.cacheKeyOwnedLatest("agent-latest", owner, slug)
	return cachedLatest(ctx, s.e, key, func() (*Agent, error) {
		return queryAgentLatest(ctx, s.e.db, s.e.prefix, owner, slug)
	})
}

// GetByID is deliberately not owner-scoped — see AgentRegistry.GetByID.
func (s *agentService) GetByID(ctx context.Context, id uuid.UUID) (*Agent, error) {
	key := s.e.cacheKey("agent-id", id.String(), 0)
	if a, ok := cacheGet[Agent](ctx, s.e.cache, key); ok {
		return a, nil
	}
	a, err := queryAgentByID(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, a, s.e.cacheTTL)
	return a, nil
}

func (s *agentService) Create(ctx context.Context, a *Agent) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	if err := insertAgent(ctx, s.e.db, s.e.prefix, a); err != nil {
		return err
	}
	// A new version may be the newest, so the cached "latest" pointer for this
	// owner+slug is now potentially stale — evict it.
	cacheDelete(ctx, s.e.cache, s.e.cacheKeyOwnedLatest("agent-latest", a.Owner, a.Slug))
	return nil
}

func (s *agentService) List(ctx context.Context, owner, category string) ([]*Agent, error) {
	return listAgents(ctx, s.e.db, s.e.prefix, owner, category)
}

// Versions is not cached — a new version may be created at any time.
func (s *agentService) Versions(ctx context.Context, owner, slug string) ([]*Agent, error) {
	return queryAgentVersions(ctx, s.e.db, s.e.prefix, owner, slug)
}

func (s *agentService) Delete(ctx context.Context, owner, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete agent %q: an explicit version >= 1 is required", slug)
	}
	// Resolve the row id first so we can also evict the by-id cache that GetByID
	// populates (cacheKey "agent-id").
	var id uuid.UUID
	if a, err := queryAgent(ctx, s.e.db, s.e.prefix, owner, slug, version); err == nil {
		id = a.ID
	}
	if err := sqlDeleteAgent(ctx, s.e.db, s.e.prefix, owner, slug, version); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, s.e.cacheKeyOwned("agent", owner, slug, version))
	// Deleting the newest version moves the latest pointer, so evict it too.
	cacheDelete(ctx, s.e.cache, s.e.cacheKeyOwnedLatest("agent-latest", owner, slug))
	if id != uuid.Nil {
		cacheDelete(ctx, s.e.cache, s.e.cacheKey("agent-id", id.String(), 0))
	}
	return nil
}

// promptService implements PromptRegistry.
