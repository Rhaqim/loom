package engine

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
	Inputs         map[string]any    // per-step inputs (e.g. a turn's lead-agent output)
	Overrides      map[string]any    // per-request provider overrides (api key, model, …)
	// ParamsMap carries free-form tuning knobs as a map — both model params
	// (temperature, max_tokens, top_p, seed, …, which the engine also folds into
	// the typed Params above) and domain params (e.g. tension). Generators read
	// whatever keys they understand.
	ParamsMap map[string]any
	// Bus is the per-turn cross-agent communication fabric (nil outside a turn).
	// Generators may Publish/Subscribe to coordinate with sibling agents.
	Bus Bus
}

// Generator produces a Result from a GenerateRequest. One per modality+provider.
//
// A generator comes in one of three flavors — implement the interface(s) that
// fit your provider; the engine detects the optional ones at runtime:
//
//   - Sync (this interface): Generate returns the finished Result. This is the
//     minimum; every generator implements it. Register with Config.Generators or
//     Engine.RegisterGenerator and point an agent at it by slug.
//   - Streaming (also implement StreamingGenerator): the engine streams chunks
//     to StepRequest.OnChunk when a caller asks for streaming, and falls back to
//     Generate otherwise.
//   - Async (also implement AsyncGenerator): Generate returns a pending Result
//     (NewPendingResult) referencing an external job; the engine's background
//     poller then calls Poll until the job resolves. Used for slow image/video
//     providers (see generator/replicate and generator/runway).
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

// AsyncGenerator extends Generator for providers whose jobs complete out of band
// (typically image/video). Generate should submit the job and return a pending
// Result (see NewPendingResult) carrying the external handle; the engine's
// background poller then calls Poll on an interval until it returns a terminal
// (ready or failed) Result. Return a still-pending Result to be polled again, or
// an error to have the attempt counted toward the poll cap. Enable the poller
// via Config.AsyncPoller.
type AsyncGenerator interface {
	Generator
	Poll(ctx context.Context, handle TaskHandle) (Result, error)
}

// PollerConfig configures the background async result poller.
type PollerConfig struct {
	Interval time.Duration // how often to poll pending tasks
	Workers  int           // number of concurrent poll goroutines
	// MaxAttempts caps how many times a task is polled before it is terminally
	// failed, so neither an erroring nor a perpetually-pending job is polled
	// forever. Zero uses defaultMaxPollAttempts.
	MaxAttempts int
}
