package loom

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type promptService struct{ e *Engine }

func (s *promptService) Get(ctx context.Context, slug string, version int) (*Prompt, error) {
	if version == 0 {
		return s.Latest(ctx, slug)
	}
	// Cache check — prompts are immutable after creation.
	key := cacheKey("prompt", s.e.prefix, slug, version)
	if p, ok := cacheGet[Prompt](ctx, s.e.cache, key); ok {
		return p, nil
	}
	p, err := queryPrompt(ctx, s.e.db, s.e.prefix, slug, version)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, p)
	return p, nil
}

func (s *promptService) Latest(ctx context.Context, slug string) (*Prompt, error) {
	return queryPromptLatest(ctx, s.e.db, s.e.prefix, slug)
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

func (s *promptService) List(ctx context.Context, kind PromptKind, category string) ([]*Prompt, error) {
	return listPrompts(ctx, s.e.db, s.e.prefix, kind, category)
}

func (s *promptService) Delete(ctx context.Context, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete prompt %q: an explicit version >= 1 is required", slug)
	}
	// Resolve the row id first so we can also evict the by-id cache that the
	// agent-build path populates (cacheKey "prompt-id").
	var id uuid.UUID
	if p, err := queryPrompt(ctx, s.e.db, s.e.prefix, slug, version); err == nil {
		id = p.ID
	}
	if err := sqlDeletePrompt(ctx, s.e.db, s.e.prefix, slug, version); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, cacheKey("prompt", s.e.prefix, slug, version))
	if id != uuid.Nil {
		cacheDelete(ctx, s.e.cache, cacheKey("prompt-id", s.e.prefix, id.String(), 0))
	}
	return nil
}

// sessionService implements SessionRegistry.
