package loom

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type costService struct{ e *Engine }

func (c *costService) SessionUsage(ctx context.Context, sessionID uuid.UUID) (*UsageSummary, error) {
	return sqlSessionUsage(ctx, c.e.db, c.e.prefix, sessionID)
}

func (c *costService) Usage(ctx context.Context, q UsageQuery) (*UsageSummary, error) {
	return sqlUsage(ctx, c.e.db, c.e.prefix, q)
}

func (c *costService) ByAgent(ctx context.Context, q UsageQuery) ([]AgentUsage, error) {
	return sqlByAgent(ctx, c.e.db, c.e.prefix, q)
}

func (c *costService) AgentStats(ctx context.Context, agentID uuid.UUID, window time.Duration) (*AgentCostStats, error) {
	return sqlAgentStats(ctx, c.e.db, c.e.prefix, agentID, window)
}

func (c *costService) Estimate(_ context.Context, req EstimateRequest) (*CostEstimate, error) {
	// Simple estimation: count input tokens naively (1 token ≈ 4 chars).
	inputTokens := len(req.Input) / 4
	// Use conservative output token estimates by modality.
	outputEst := 0
	switch req.Modal {
	case ModalityText, ModalityStructured:
		outputEst = 512
	}
	return &CostEstimate{
		USDLow:                float64(inputTokens) * 0.000001,
		USDHigh:               float64(inputTokens+outputEst) * 0.000030,
		InputTokens:           inputTokens,
		OutputTokensEstimated: outputEst,
	}, nil
}

func (c *costService) Record(ctx context.Context, rec CostRecord) error {
	return sqlInsertCostRecord(ctx, c.e.db, c.e.prefix, rec)
}

func (c *costService) recordFromResult(ctx context.Context, step *Step, agent *Agent, result Result, platformID string) {
	var (
		inputTokens  int
		outputTokens int
		usdCost      float64
	)
	switch r := result.(type) {
	case *TextResult:
		inputTokens = r.InputTokens
		outputTokens = r.OutputTokens
	case *StructuredResult:
		inputTokens = r.InputTokens
		outputTokens = r.OutputTokens
	}
	// Per-model pricing: the generator reports its model on result metadata;
	// fall back to the generator slug as the pricing key.
	model := ResultModel(result)
	if model == "" {
		model = agent.GeneratorSlug
	}
	usdCost = c.e.priceFor(model).Cost(inputTokens, outputTokens)

	rec := CostRecord{
		Provider:     agent.GeneratorSlug,
		Model:        model,
		Modal:        agent.Modal,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		USDCost:      usdCost,
		StepID:       step.ID,
		AgentID:      agent.ID,
		SessionID:    step.SessionID,
		PlatformID:   platformID,
		Timestamp:    time.Now(),
	}
	if err := c.Record(ctx, rec); err != nil {
		c.e.log.Error("record cost", "step_id", step.ID, "err", err)
	}
}

// budgetService implements BudgetManager and the budget pre-hook.
