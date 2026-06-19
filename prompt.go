package loom

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
	Version   int    // monotonically increasing; editing creates a new version
	Kind      PromptKind
	Category  string   // platform-defined grouping
	Body      string   // raw template text
	Variables []string // documented input variable names
	Metadata  map[string]any
	// ResponseFormat is the expected output schema for agents driven by this
	// prompt. It is stored in the DB alongside the prompt so the prompt and its
	// output contract travel together: when an agent's own ResponseFormat is nil,
	// the engine uses the system prompt's ResponseFormat for generation. Most
	// useful on system prompts (Kind == PromptKindSystem).
	ResponseFormat *ResponseFormat
	CreatedAt      time.Time
	Notes          string // design rationale, change notes
}

// ResponseFormatJSON builds a ResponseFormat from a raw JSON Schema string — the
// easy path for attaching an output schema to a prompt (paste the schema a client
// authored straight into the DB). An empty/blank raw yields a non-nil
// ResponseFormat with no schema, which generators treat as portable JSON mode.
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
