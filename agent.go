package loom

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Agent is a configuration bundling a Generator, system prompt, user template,
// response format, and generation params. It is the unit the platform spawns.
type Agent struct {
	ID             uuid.UUID
	Slug           string    // human-readable identifier
	Version        int       // monotonically increasing
	Category       string    // platform-defined grouping
	Modal          Modality  // the modality this agent produces
	GeneratorSlug  string    // references a registered Generator
	SystemPromptID uuid.UUID // FK → loom_prompts (system)
	UserTemplateID uuid.UUID // FK → loom_prompts (user_template)
	// ResponseFormatID references a reusable response-format record
	// (loom_response_formats). When set it is the agent's output contract and is
	// shared across agents/versions. A nil ID falls back to the inline
	// ResponseFormat below (ad-hoc, copied per agent).
	ResponseFormatID *uuid.UUID
	ResponseFormat   *ResponseFormat
	Params           GenerateParams
	FallbackAgentID  *uuid.UUID // used when budget action = downgrade
	CreatedAt        time.Time
}

// AgentRegistry resolves and manages agents.
type AgentRegistry interface {
	// Get retrieves a specific version. Version 0 resolves to LATEST.
	Get(ctx context.Context, slug string, version int) (*Agent, error)
	// Latest resolves the highest version of slug.
	Latest(ctx context.Context, slug string) (*Agent, error)
	// Create persists a new agent version.
	Create(ctx context.Context, a *Agent) error
	// List returns all agents matching optional category filter.
	List(ctx context.Context, category string) ([]*Agent, error)
}
