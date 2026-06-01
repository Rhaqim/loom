package loom

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TaskHandle references an in-flight async provider job.
type TaskHandle struct {
	ID       uuid.UUID
	Provider string
	Handle   string // external job ID (e.g. Runway run ID)
}

// GenerateParams holds per-call tuning knobs for the generator.
type GenerateParams struct {
	Temperature      float64
	MaxTokens        int
	TopP             float64
	FrequencyPenalty float64
	PresencePenalty  float64
	Seed             int
	// Image / video specific
	Width       int
	Height      int
	DurationSec float64
	// Extra provider-specific params (merged last)
	Extra map[string]any
}

// ResponseFormat describes the expected JSON schema for structured output.
type ResponseFormat struct {
	Schema     map[string]any // JSON Schema
	StrictMode bool
}

// GenerateRequest is the input to a Generator.
type GenerateRequest struct {
	SystemPrompt   string
	UserPrompt     string
	Params         GenerateParams
	ResponseFormat *ResponseFormat // nil unless ModalityStructured
	Context        []Result        // prior modality results for continuity
	SessionID      uuid.UUID
	AgentID        uuid.UUID
	Annotations    []RetryAnnotation // retry hints from post-hooks
}

// Generator produces a Result from a GenerateRequest. One per modality+provider.
type Generator interface {
	Modality() Modality
	Generate(ctx context.Context, req GenerateRequest) (Result, error)
}

// Chunk carries a partial text fragment from a streaming generator.
type Chunk struct {
	Content string
	Tokens  int // tokens in this chunk (provider-dependent, may be 0)
}

// StreamingGenerator extends Generator with incremental chunk delivery.
// Only text-modality generators need to implement this interface.
type StreamingGenerator interface {
	Generator
	// GenerateStream streams chunks on the first channel.
	// The final assembled Result arrives on the second channel after streaming completes.
	GenerateStream(ctx context.Context, req GenerateRequest) (<-chan Chunk, <-chan Result, error)
}

// PollerConfig configures the background async result poller.
type PollerConfig struct {
	Interval time.Duration // how often to poll pending tasks
	Workers  int           // number of concurrent poll goroutines
}
