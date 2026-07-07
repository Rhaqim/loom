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
	// Estimate returns a pre-flight cost estimate for a step request.
	Estimate(ctx context.Context, req EstimateRequest) (*CostEstimate, error)
	// Record persists a cost record.
	Record(ctx context.Context, rec CostRecord) error
}

// UsageSummary aggregates cost metrics.
type UsageSummary struct {
	TotalUSD    float64
	TotalTokens int
	StepCount   int
	ByModel     map[string]float64
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

// AgentCostStats holds statistical cost data for an agent.
type AgentCostStats struct {
	USDMean   float64
	USDP50    float64
	USDP95    float64
	TokensP50 int
	TokensP95 int
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
