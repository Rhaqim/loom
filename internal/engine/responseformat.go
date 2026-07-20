package engine

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
	Owner      string // opaque app-owned scope ("" = global)
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

// ResponseFormatRegistry resolves and manages reusable response formats. owner
// is an opaque scope the embedding application controls (e.g. a tenant/studio
// id); "" is the global default. Slugs are only unique within an owner, so every
// slug-addressed lookup takes one.
type ResponseFormatRegistry interface {
	// Create persists a new response-format version. The scope comes from
	// rf.Owner.
	Create(ctx context.Context, rf *ResponseFormatRecord) error
	// Get retrieves a specific version within owner. Version 0 resolves to LATEST.
	Get(ctx context.Context, owner, slug string, version int) (*ResponseFormatRecord, error)
	// Latest resolves the highest version of slug within owner.
	Latest(ctx context.Context, owner, slug string) (*ResponseFormatRecord, error)
	// GetByID retrieves a response format by ID.
	//
	// This is NOT owner-scoped: it exists to resolve loom's own internal FK
	// references (Agent.ResponseFormatID), and a UUID primary key is globally
	// unique. An application must therefore not pass an untrusted,
	// tenant-supplied ID to it without checking the returned record's Owner
	// against the caller's scope itself.
	GetByID(ctx context.Context, id uuid.UUID) (*ResponseFormatRecord, error)
	// Delete removes a specific response-format version (>= 1) within owner. On
	// Postgres it fails if the version is still referenced by an agent (ON DELETE
	// RESTRICT).
	Delete(ctx context.Context, owner, slug string, version int) error
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

func (s *responseFormatService) Get(ctx context.Context, owner, slug string, version int) (*ResponseFormatRecord, error) {
	if version == 0 {
		return s.Latest(ctx, owner, slug)
	}
	key := cacheKeyOwned("rf", s.e.prefix, owner, slug, version)
	if r, ok := cacheGet[ResponseFormatRecord](ctx, s.e.cache, key); ok {
		return r, nil
	}
	r, err := sqlQueryResponseFormat(ctx, s.e.db, s.e.prefix, owner, slug, version)
	if err != nil {
		return nil, wrapNotFound(err, "response_format", fmt.Sprintf("%s@v%d", slug, version))
	}
	cacheSet(ctx, s.e.cache, key, r, s.e.cacheTTL)
	return r, nil
}

func (s *responseFormatService) Latest(ctx context.Context, owner, slug string) (*ResponseFormatRecord, error) {
	r, err := sqlQueryResponseFormatLatest(ctx, s.e.db, s.e.prefix, owner, slug)
	return r, wrapNotFound(err, "response_format", slug)
}

// GetByID is deliberately not owner-scoped — see ResponseFormatRegistry.GetByID.
func (s *responseFormatService) GetByID(ctx context.Context, id uuid.UUID) (*ResponseFormatRecord, error) {
	key := cacheKey("rf-id", s.e.prefix, id.String(), 0)
	if r, ok := cacheGet[ResponseFormatRecord](ctx, s.e.cache, key); ok {
		return r, nil
	}
	r, err := sqlQueryResponseFormatByID(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, wrapNotFound(err, "response_format", id.String())
	}
	cacheSet(ctx, s.e.cache, key, r, s.e.cacheTTL)
	return r, nil
}

func (s *responseFormatService) Delete(ctx context.Context, owner, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete response format %q: an explicit version >= 1 is required", slug)
	}
	// Resolve the row id first so we can also evict the by-id cache that GetByID
	// and the agent-build path populate (cacheKey "rf-id").
	var id uuid.UUID
	if rf, err := sqlQueryResponseFormat(ctx, s.e.db, s.e.prefix, owner, slug, version); err == nil {
		id = rf.ID
	}
	if err := sqlDeleteResponseFormat(ctx, s.e.db, s.e.prefix, owner, slug, version); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, cacheKeyOwned("rf", s.e.prefix, owner, slug, version))
	if id != uuid.Nil {
		cacheDelete(ctx, s.e.cache, cacheKey("rf-id", s.e.prefix, id.String(), 0))
	}
	return nil
}
