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

	// Annotations carried from previous retry attempts.
	annotations []RetryAnnotation
}

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
