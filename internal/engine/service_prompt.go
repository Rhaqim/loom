package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type promptService struct{ e *Engine }

func (s *promptService) Get(ctx context.Context, owner, slug string, version int) (*Prompt, error) {
	if version == 0 {
		return s.Latest(ctx, owner, slug)
	}
	// Cache check — prompts are immutable after creation. The key carries owner:
	// slugs are unique only within one, so an unowned key would leak a record
	// across tenants.
	key := cacheKeyOwned("prompt", s.e.prefix, owner, slug, version)
	if p, ok := cacheGet[Prompt](ctx, s.e.cache, key); ok {
		return p, nil
	}
	p, err := queryPrompt(ctx, s.e.db, s.e.prefix, owner, slug, version)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, p, s.e.cacheTTL)
	return p, nil
}

func (s *promptService) Latest(ctx context.Context, owner, slug string) (*Prompt, error) {
	return queryPromptLatest(ctx, s.e.db, s.e.prefix, owner, slug)
}

// GetByID is deliberately not owner-scoped — see PromptRegistry.GetByID.
func (s *promptService) GetByID(ctx context.Context, id uuid.UUID) (*Prompt, error) {
	key := cacheKey("prompt-id", s.e.prefix, id.String(), 0)
	if p, ok := cacheGet[Prompt](ctx, s.e.cache, key); ok {
		return p, nil
	}
	p, err := queryPromptByID(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, p, s.e.cacheTTL)
	return p, nil
}

func (s *promptService) Create(ctx context.Context, p *Prompt) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	return insertPrompt(ctx, s.e.db, s.e.prefix, p)
}

func (s *promptService) List(ctx context.Context, owner string, kind PromptKind, category string) ([]*Prompt, error) {
	return listPrompts(ctx, s.e.db, s.e.prefix, owner, kind, category)
}

// Versions is not cached — a new version may be created at any time.
func (s *promptService) Versions(ctx context.Context, owner, slug string) ([]*Prompt, error) {
	return queryPromptVersions(ctx, s.e.db, s.e.prefix, owner, slug)
}

func (s *promptService) Delete(ctx context.Context, owner, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete prompt %q: an explicit version >= 1 is required", slug)
	}
	// Resolve the row id first so we can also evict the by-id cache that the
	// agent-build path populates (cacheKey "prompt-id").
	var id uuid.UUID
	if p, err := queryPrompt(ctx, s.e.db, s.e.prefix, owner, slug, version); err == nil {
		id = p.ID
	}
	if err := sqlDeletePrompt(ctx, s.e.db, s.e.prefix, owner, slug, version); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, cacheKeyOwned("prompt", s.e.prefix, owner, slug, version))
	if id != uuid.Nil {
		cacheDelete(ctx, s.e.cache, cacheKey("prompt-id", s.e.prefix, id.String(), 0))
	}
	return nil
}

// sessionService implements SessionRegistry.
