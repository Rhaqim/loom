package loom

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// StoredRequest is a serialized GenerateRequest persisted alongside the step.
type StoredRequest struct {
	SystemPrompt   string          `json:"system_prompt"`
	UserPrompt     string          `json:"user_prompt"`
	Params         GenerateParams  `json:"params"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// StoredResult is a modality-typed result payload persisted in loom_results.
type StoredResult struct {
	Modality Modality        `json:"modality"`
	Status   ResultStatus    `json:"status"`
	Payload  json.RawMessage `json:"payload"`
	TaskID   *uuid.UUID      `json:"task_id,omitempty"`
}

// Step is one iteration of the engine loop: build request → call agent → result → actions.
// Steps are append-only; they form the complete timeline of a session.
type Step struct {
	ID          uuid.UUID
	SessionID   uuid.UUID
	Index       int
	AgentID     uuid.UUID
	Request     StoredRequest
	Result      Result
	Action      *Action // the discriminator's action that triggered this step (nil for step 0)
	Annotations []EntityAnnotation
	Diagnostics map[string]any // anything pre/post processors logged
	DurationMs  int
	CreatedAt   time.Time
}

// StepRequest is the input to RunStep.
type StepRequest struct {
	// Agent is the agent to run. Exactly one of Agent or AgentSlug must be set.
	Agent        *Agent
	AgentSlug    string // resolved to LATEST if Version is 0
	AgentVersion int

	// Action is the discriminator's input; nil for the opening step.
	Action *Action

	// Callbacks (all optional)
	OnChunk      func(Chunk)      // receives streaming text fragments
	OnCostUpdate func(CostRecord) // receives incremental cost updates during streaming

	// MaxRetries caps the number of post-hook retry attempts (default 3).
	MaxRetries int

	// Session is the session this step runs against. The engine populates it
	// before pre-hooks run so hooks can read and mutate session state — e.g. a
	// memory-recall pre-hook can inject facts into State.Vars *before* the user
	// template is rendered. It is set automatically by RunStep; callers may
	// leave it nil.
	Session *Session

	// Inputs are arbitrary values made available to the user template as
	// {{.Inputs.<key>}} and forwarded to the generator. RunTurn uses this to
	// hand a lead agent's output to follower agents within the same turn.
	Inputs map[string]any

	// GeneratorOverride, when non-empty, replaces the agent's GeneratorSlug for
	// this call only — a per-request provider override (e.g. a user-selected
	// model routed to a different registered generator).
	GeneratorOverride string

	// ParamOverride, when non-nil, replaces the agent's generation params for
	// this call only.
	ParamOverride *GenerateParams

	// SystemPromptOverride, when non-nil, replaces the agent's system prompt for
	// this call only — resolved from the registry (Slug+Version) or a file
	// (File). Used by the test harness to compare system-prompt variants.
	SystemPromptOverride *PromptRef

	// Overrides is opaque per-request data passed through to the generator on
	// GenerateRequest.Overrides — e.g. {"api_key": "...", "model": "..."} for
	// per-user key routing in a custom generator.
	Overrides map[string]any

	// ResponseFormat is the output contract resolved for this step (from the
	// agent or its system prompt). The engine sets it before hooks run so a
	// post-hook can validate the result against the schema.
	ResponseFormat *ResponseFormat

	// Params is a free-form map of tuning knobs for this call: model params
	// (temperature, max_tokens, top_p, seed, frequency_penalty, presence_penalty,
	// width, height, duration_sec) are folded into the typed generation params,
	// and the whole map is forwarded to the generator (GenerateRequest.ParamsMap)
	// and template ({{.Params.x}}) so domain knobs like "tension" flow through.
	Params map[string]any

	// Annotations carried from previous retry attempts.
	annotations []RetryAnnotation

	// turn linkage — set by RunTurn so persisted steps can be grouped into a
	// single logical turn for branching, replay, and cost rollups.
	turnID   uuid.UUID
	turnRole string
	// bus is the per-turn cross-agent channel fabric, set by RunTurn.
	bus Bus
}

// Bus returns the per-turn cross-agent communication fabric for this step, or nil
// if the step is not part of a RunTurn. Hooks and plugins use it to publish or
// subscribe alongside the turn's agents.
func (r *StepRequest) Bus() Bus { return r.bus }

// StepRunner executes a step request against a session.
type StepRunner interface {
	RunStep(ctx context.Context, session *Session, req StepRequest) (*Step, error)
}

// CostRecord is a lightweight view of a cost entry used in callbacks.
// The full type lives in the cost sub-package; this alias avoids an import cycle.
type CostRecord struct {
	Provider     string
	Model        string
	Modal        Modality
	InputTokens  int
	OutputTokens int
	Images       int
	DurationSec  float64
	USDCost      float64
	Estimated    bool
	StepID       uuid.UUID
	AgentID      uuid.UUID
	SessionID    uuid.UUID
	PlatformID   string
	Tags         []string
	Timestamp    time.Time
}
