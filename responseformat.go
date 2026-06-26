package loom

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// responseformat.go makes a response format a first-class, versioned, REUSABLE
// record. An agent points at one by ID (Agent.ResponseFormatID), so the same
// schema is stored once and shared: a new system-prompt version composes a new
// agent that references the SAME response-format record — the contract is reused,
// not copied. (An agent may instead carry an inline ResponseFormat for ad-hoc
// use; a non-nil ResponseFormatID wins.)

// ResponseFormatRecord is a stored, versioned response format.
type ResponseFormatRecord struct {
	ID         uuid.UUID
	Slug       string // human-readable identifier, e.g. "logician-schema"
	Owner      string // opaque app-owned scope ("" = global); reserved for future per-tenant use
	Version    int    // monotonically increasing; editing creates a new version
	Schema     map[string]any
	StrictMode bool
	CreatedAt  time.Time
}

// Format returns the value form used in a GenerateRequest.
func (r *ResponseFormatRecord) Format() *ResponseFormat {
	if r == nil {
		return nil
	}
	return &ResponseFormat{Schema: r.Schema, StrictMode: r.StrictMode}
}

// ResponseFormatRegistry resolves and manages reusable response formats.
type ResponseFormatRegistry interface {
	// Create persists a new response-format version.
	Create(ctx context.Context, rf *ResponseFormatRecord) error
	// Get retrieves a specific version. Version 0 resolves to LATEST.
	Get(ctx context.Context, slug string, version int) (*ResponseFormatRecord, error)
	// Latest resolves the highest version of slug.
	Latest(ctx context.Context, slug string) (*ResponseFormatRecord, error)
	// GetByID retrieves a response format by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*ResponseFormatRecord, error)
	// Delete removes a specific response-format version (>= 1). On Postgres it
	// fails if the version is still referenced by an agent (ON DELETE RESTRICT).
	Delete(ctx context.Context, slug string, version int) error
}

// ResponseFormats returns the response-format registry.
func (e *Engine) ResponseFormats() ResponseFormatRegistry { return e.responseFormats }

type responseFormatService struct{ e *Engine }

func (s *responseFormatService) Create(ctx context.Context, rf *ResponseFormatRecord) error {
	if rf.ID == uuid.Nil {
		rf.ID = uuid.New()
	}
	if rf.CreatedAt.IsZero() {
		rf.CreatedAt = time.Now()
	}
	return sqlInsertResponseFormat(ctx, s.e.db, s.e.prefix, rf)
}

func (s *responseFormatService) Get(ctx context.Context, slug string, version int) (*ResponseFormatRecord, error) {
	if version == 0 {
		return s.Latest(ctx, slug)
	}
	key := cacheKey("rf", s.e.prefix, slug, version)
	if r, ok := cacheGet[ResponseFormatRecord](ctx, s.e.cache, key); ok {
		return r, nil
	}
	r, err := sqlQueryResponseFormat(ctx, s.e.db, s.e.prefix, slug, version)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, r)
	return r, nil
}

func (s *responseFormatService) Latest(ctx context.Context, slug string) (*ResponseFormatRecord, error) {
	return sqlQueryResponseFormatLatest(ctx, s.e.db, s.e.prefix, slug)
}

func (s *responseFormatService) GetByID(ctx context.Context, id uuid.UUID) (*ResponseFormatRecord, error) {
	key := cacheKey("rf-id", s.e.prefix, id.String(), 0)
	if r, ok := cacheGet[ResponseFormatRecord](ctx, s.e.cache, key); ok {
		return r, nil
	}
	r, err := sqlQueryResponseFormatByID(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, r)
	return r, nil
}

func (s *responseFormatService) Delete(ctx context.Context, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete response format %q: an explicit version >= 1 is required", slug)
	}
	// Resolve the row id first so we can also evict the by-id cache that GetByID
	// and the agent-build path populate (cacheKey "rf-id").
	var id uuid.UUID
	if rf, err := sqlQueryResponseFormat(ctx, s.e.db, s.e.prefix, slug, version); err == nil {
		id = rf.ID
	}
	if err := sqlDeleteResponseFormat(ctx, s.e.db, s.e.prefix, slug, version); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, cacheKey("rf", s.e.prefix, slug, version))
	if id != uuid.Nil {
		cacheDelete(ctx, s.e.cache, cacheKey("rf-id", s.e.prefix, id.String(), 0))
	}
	return nil
}
