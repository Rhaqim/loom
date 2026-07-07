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

func (s *agentService) Get(ctx context.Context, slug string, version int) (*Agent, error) {
	if version == 0 {
		return s.Latest(ctx, slug)
	}
	// Cache check — agents are immutable after creation.
	key := cacheKey("agent", s.e.prefix, slug, version)
	if a, ok := cacheGet[Agent](ctx, s.e.cache, key); ok {
		return a, nil
	}
	a, err := queryAgent(ctx, s.e.db, s.e.prefix, slug, version)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, a, s.e.cacheTTL)
	return a, nil
}

func (s *agentService) Latest(ctx context.Context, slug string) (*Agent, error) {
	return queryAgentLatest(ctx, s.e.db, s.e.prefix, slug)
}

func (s *agentService) Create(ctx context.Context, a *Agent) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	return insertAgent(ctx, s.e.db, s.e.prefix, a)
}

func (s *agentService) List(ctx context.Context, category string) ([]*Agent, error) {
	return listAgents(ctx, s.e.db, s.e.prefix, category)
}

func (s *agentService) Delete(ctx context.Context, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete agent %q: an explicit version >= 1 is required", slug)
	}
	if err := sqlDeleteAgent(ctx, s.e.db, s.e.prefix, slug, version); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, cacheKey("agent", s.e.prefix, slug, version))
	return nil
}

// promptService implements PromptRegistry.
