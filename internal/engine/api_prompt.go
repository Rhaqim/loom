package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Prompt is a versioned, categorized text template stored in the database.
type Prompt struct {
	ID        uuid.UUID
	Slug      string // human-readable identifier, e.g. "opening-author"
	Owner     string // opaque app-owned scope ("" = global); reserved for future per-tenant use
	Version   int    // monotonically increasing; editing creates a new version
	Kind      PromptKind
	Category  string   // platform-defined grouping
	Body      string   // raw template text
	Variables []string // documented input variable names
	Metadata  map[string]any
	CreatedAt time.Time
	Notes     string // design rationale, change notes
}

// ResponseFormatJSON builds a ResponseFormat from a raw JSON Schema string — the
// easy path for attaching an output schema to an agent (paste a client-authored
// schema straight in). An empty/blank raw yields a non-nil ResponseFormat with no
// schema, which generators treat as portable JSON mode. Response formats are
// independent of prompts: the same one can be reused across many agents, and an
// agent's system prompt, user template, and response format are each versioned
// and referenced independently.
func ResponseFormatJSON(rawSchema string, strict bool) (*ResponseFormat, error) {
	rf := &ResponseFormat{StrictMode: strict}
	if s := strings.TrimSpace(rawSchema); s != "" {
		if err := json.Unmarshal([]byte(s), &rf.Schema); err != nil {
			return nil, fmt.Errorf("loom: response format schema: %w", err)
		}
	}
	return rf, nil
}

// MustResponseFormatJSON is ResponseFormatJSON that panics on invalid JSON —
// convenient for static schemas defined in code.
func MustResponseFormatJSON(rawSchema string, strict bool) *ResponseFormat {
	rf, err := ResponseFormatJSON(rawSchema, strict)
	if err != nil {
		panic(err)
	}
	return rf
}

// PromptKind discriminates between system and user template prompts.
type PromptKind string

const (
	PromptKindSystem       PromptKind = "system"
	PromptKindUserTemplate PromptKind = "user_template"
)

// PromptRef is a lightweight pointer to a specific prompt version.
type PromptRef struct {
	Slug    string
	Version int    // 0 = LATEST
	File    string // non-empty → load from file instead of DB
	// Literal, when non-empty, is used verbatim and takes precedence over Slug
	// and File. It is the seam for embedders that assemble a system prompt per
	// turn (from world state, memory, etc.) rather than storing it in the DB.
	Literal string
}

// PromptByName constructs a PromptRef for a specific slug+version.
func PromptByName(slug string, version int) PromptRef {
	return PromptRef{Slug: slug, Version: version}
}

// PromptFromFile constructs a PromptRef loaded from a file path.
func PromptFromFile(path string) PromptRef {
	return PromptRef{File: path}
}

// PromptRegistry resolves and manages prompts.
type PromptRegistry interface {
	// Get retrieves a specific version. Version 0 resolves to LATEST. A missing
	// prompt returns *NotFoundError (which unwraps to ErrNotFound).
	Get(ctx context.Context, slug string, version int) (*Prompt, error)
	// Latest resolves the highest version of slug.
	Latest(ctx context.Context, slug string) (*Prompt, error)
	// Create persists a new prompt version.
	Create(ctx context.Context, p *Prompt) error
	// List returns all prompts matching kind and optional category filter.
	List(ctx context.Context, kind PromptKind, category string) ([]*Prompt, error)
	// Delete removes a specific prompt version (>= 1). On Postgres it fails if the
	// version is still referenced by an agent (ON DELETE RESTRICT).
	Delete(ctx context.Context, slug string, version int) error
}
