package engine

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
	ID        uuid.UUID
	SessionID uuid.UUID
	// Index is the step's position within its session, assigned by the engine
	// from the persisted maximum inside the step's own transaction. It is
	// contiguous from 0 and unique per session — never across sessions, since a
	// forked branch restarts at 0. Join on (SessionID, Index), never Index alone.
	//
	// It is an output: setting it on a StepRequest has no effect.
	Index       int
	AgentID     uuid.UUID
	Request     StoredRequest
	Result      Result
	Action      *Action        // the discriminator's action that triggered this step (nil for step 0)
	Diagnostics map[string]any // anything pre/post processors logged
	DurationMs  int
	CreatedAt   time.Time
}

// RetryMode selects how the engine treats post-hook retries.
type RetryMode int

const (
	// RetryDiscard re-runs on a post-hook retry and discards the rejected
	// attempt; exhausting the retry budget fails the step. This is the default.
	RetryDiscard RetryMode = iota
	// RetryKeepBest scores every attempt (post-hooks return ErrRetryScored) and
	// persists the lowest-scoring draft. Exhausting the budget keeps the best
	// draft instead of failing, a generator failure after attempt 0 keeps the
	// best draft so far, and only attempt 0 streams (retries run silently).
	RetryKeepBest
)

// StepRequest is the input to RunStep.
type StepRequest struct {
	// Agent is the agent to run. Exactly one of Agent or AgentSlug must be set.
	Agent        *Agent
	AgentSlug    string // resolved to LATEST if Version is 0
	AgentVersion int

	// Owner scopes every registry lookup this step performs — the agent named
	// by AgentSlug and any prompt resolved by slug through
	// SystemPromptOverride. It is the same opaque app-owned scope carried on
	// Agent.Owner / Prompt.Owner; "" is the global scope.
	//
	// A multi-tenant caller MUST set this. Leaving it "" resolves against the
	// global scope, which will simply not find a tenant's records rather than
	// silently reaching across tenants. Owner is NOT consulted when Agent is
	// supplied directly, since that bypasses resolution entirely — the caller
	// is responsible for having loaded a record it is entitled to.
	Owner string

	// Action is the discriminator's input; nil for the opening step.
	Action *Action

	// Callbacks (all optional)
	OnChunk      func(Chunk)      // receives streaming text fragments
	OnCostUpdate func(CostRecord) // receives incremental cost updates during streaming
	// OnStreamEnd fires when a streaming attempt's chunk channel closes, before
	// post-hooks run, carrying the 0-based attempt index. It lets a caller close
	// its chunk sink the moment the streamed draft is complete, even when a
	// silent RetryKeepBest retry follows.
	OnStreamEnd func(attempt int)

	// MaxRetries caps the number of post-hook retry attempts (default 3).
	MaxRetries int
	// RetryMode selects retry semantics (default RetryDiscard). See RetryMode.
	RetryMode RetryMode

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
	// attempt is the 0-based index of the current generation attempt, set by the
	// engine before each post-hook run so a hook can self-cap its own retries.
	attempt int

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

// Attempt returns the 0-based index of the current generation attempt. A
// post-hook reads it to self-cap how many times it re-requests its own
// correction before accepting a draft.
func (r *StepRequest) Attempt() int { return r.attempt }

// RetryAnnotations returns a copy of the retry hints accumulated across prior
// attempts of this step, so a hook can count how often its own annotation Kind
// has already fired.
func (r *StepRequest) RetryAnnotations() []RetryAnnotation {
	if len(r.annotations) == 0 {
		return nil
	}
	out := make([]RetryAnnotation, len(r.annotations))
	copy(out, r.annotations)
	return out
}

// TurnRole is "lead" or "follower:<slug>" when the step runs inside a RunTurn,
// and "" for a standalone RunStep. Hooks use it to behave differently for the
// streaming lead versus a parallel follower.
func (r *StepRequest) TurnRole() string { return r.turnRole }

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
