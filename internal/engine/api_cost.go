package engine

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CostManager is the public interface returned by Engine.Cost().
type CostManager interface {
	// SessionUsage returns aggregated cost for a single session.
	SessionUsage(ctx context.Context, sessionID uuid.UUID) (*UsageSummary, error)
	// Usage returns aggregated cost for a target over a time window.
	Usage(ctx context.Context, q UsageQuery) (*UsageSummary, error)
	// ByAgent returns per-agent cost breakdown.
	ByAgent(ctx context.Context, q UsageQuery) ([]AgentUsage, error)
	// AgentStats returns p50/p95 cost stats for a single agent.
	AgentStats(ctx context.Context, agentID uuid.UUID, window time.Duration) (*AgentCostStats, error)
	// ByStep returns per-step usage for one session, oldest first — the
	// breakdown behind SessionUsage's totals.
	//
	// Every other cost method is an aggregate, so "which agent in this turn was
	// expensive" was previously unanswerable; this is what lets a step timeline
	// show tokens and cost per generation. It is a separate call rather than a
	// field on Step because Session.Get already loads unbounded history — see
	// SessionRegistry.GetHeader — and joining costs into that read would charge
	// every session load for data most callers do not want.
	//
	// Charges with no originating step (session- or platform-level records) are
	// omitted, so these rows sum to SessionUsage only when every charge came
	// from a step.
	ByStep(ctx context.Context, sessionID uuid.UUID) ([]StepUsage, error)
	// Estimate returns a pre-flight cost estimate for a step request.
	Estimate(ctx context.Context, req EstimateRequest) (*CostEstimate, error)
	// Record persists a cost record.
	Record(ctx context.Context, rec CostRecord) error
}

// UsageSummary aggregates cost metrics.
//
// Token and step counts are always exact. The USD figures are only meaningful
// when PricingConfigured is true — see that field.
type UsageSummary struct {
	TotalUSD    float64
	TotalTokens int
	StepCount   int
	ByModel     map[string]float64

	// PricingConfigured reports whether the engine had real pricing (either
	// Config.Pricing or Config.DefaultPrice) when these costs were recorded.
	//
	// When false, TotalUSD and ByModel are derived from a built-in placeholder
	// rate and are NOT real money. Render them as "—"/unknown rather than as a
	// dollar amount, and do not use them for billing or reporting. Note that any
	// USD-denominated budget is evaluated against these same figures, so a USD
	// limit is not enforceable in this state.
	PricingConfigured bool
}

// StepUsage is one step's recorded generation usage — the per-step row behind
// the aggregates. Token counts, image counts and duration are exact.
//
// USDCost is only real money when Estimated is false; when true it was derived
// from a built-in placeholder rate because no pricing was configured (see
// UsageSummary.PricingConfigured). Render it as unknown rather than as a
// dollar amount in that case.
type StepUsage struct {
	StepID       uuid.UUID
	AgentID      uuid.UUID // uuid.Nil if the charge was not attributed to an agent
	Provider     string
	Model        string
	Modal        Modality
	InputTokens  int
	OutputTokens int
	Images       int
	DurationSec  float64
	USDCost      float64
	Estimated    bool
	Timestamp    time.Time
}

// UsageQuery is the input to cost queries.
type UsageQuery struct {
	Target BudgetTarget
	From   time.Time
	To     time.Time
	Tags   []string // prefix "!" to exclude a tag
}

// AgentUsage is a per-agent cost breakdown entry.
type AgentUsage struct {
	AgentSlug    string
	AgentVersion int
	RunCount     int
	USDTotal     float64
}

// AgentCostStats holds statistical cost data for an agent. The USD* fields are
// only real money when PricingConfigured is true; see UsageSummary.
type AgentCostStats struct {
	USDMean   float64
	USDP50    float64
	USDP95    float64
	TokensP50 int
	TokensP95 int

	// PricingConfigured — see UsageSummary.PricingConfigured.
	PricingConfigured bool
}

// EstimateRequest is the input to Cost().Estimate().
type EstimateRequest struct {
	AgentID uuid.UUID
	Input   string
	Modal   Modality
}

// CostEstimate is the output of Cost().Estimate().
type CostEstimate struct {
	USDLow                float64
	USDHigh               float64
	InputTokens           int
	OutputTokensEstimated int
}
