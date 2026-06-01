package loom

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Prompt is a versioned, categorized text template stored in the database.
type Prompt struct {
	ID        uuid.UUID
	Slug      string // human-readable identifier, e.g. "opening-author"
	Version   int    // monotonically increasing; editing creates a new version
	Kind      PromptKind
	Category  string   // platform-defined grouping
	Body      string   // raw template text
	Variables []string // documented input variable names
	Metadata  map[string]any
	CreatedAt time.Time
	Notes     string // design rationale, change notes
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
	// Get retrieves a specific version. Version 0 resolves to LATEST.
	Get(ctx context.Context, slug string, version int) (*Prompt, error)
	// Latest resolves the highest version of slug.
	Latest(ctx context.Context, slug string) (*Prompt, error)
	// Create persists a new prompt version.
	Create(ctx context.Context, p *Prompt) error
	// List returns all prompts matching kind and optional category filter.
	List(ctx context.Context, kind PromptKind, category string) ([]*Prompt, error)
}
