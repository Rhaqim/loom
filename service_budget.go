package loom

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type budgetService struct{ e *Engine }

func (b *budgetService) Create(ctx context.Context, budget *Budget) error {
	// Only platform_id budgets are enforced today; reject other kinds rather than
	// silently storing a budget that would never be applied.
	if budget.Target.Kind != TargetPlatformID {
		return fmt.Errorf("loom: budget target kind %q is not supported (only %q is enforced)", budget.Target.Kind, TargetPlatformID)
	}
	if budget.ID == uuid.Nil {
		budget.ID = uuid.New()
	}
	if budget.CreatedAt.IsZero() {
		budget.CreatedAt = time.Now()
	}
	return sqlInsertBudget(ctx, b.e.db, b.e.prefix, budget)
}

func (b *budgetService) Get(ctx context.Context, id uuid.UUID) (*Budget, error) {
	return sqlQueryBudget(ctx, b.e.db, b.e.prefix, id)
}

func (b *budgetService) List(ctx context.Context, targetKind, targetKey string) ([]*Budget, error) {
	return sqlListBudgets(ctx, b.e.db, b.e.prefix, targetKind, targetKey)
}

func (b *budgetService) Delete(ctx context.Context, id uuid.UUID) error {
	return sqlDeleteBudget(ctx, b.e.db, b.e.prefix, id)
}

// enforce checks active platform budgets and applies the configured action when
// a limit is exceeded: block (returns *BudgetExceededError), downgrade (swaps to
// the agent's fallback), or notify (logs and continues). It returns the agent to
// use.
//
// The cap is BEST-EFFORT, not a hard ceiling: it reads only PRIOR recorded spend
// (the current step's cost is recorded after the step, fire-and-forget), and a
// turn's parallel followers each see the same prior spend — so spend can overshoot
// by up to one step per concurrent call. It also fails OPEN on lookup/usage errors
// so a transient DB glitch never silently breaks a run.
func (b *budgetService) enforce(ctx context.Context, session *Session, agent *Agent) (*Agent, error) {
	if session == nil || session.PlatformID == "" {
		return agent, nil
	}
	budgets, err := b.List(ctx, TargetPlatformID, session.PlatformID)
	if err != nil {
		b.e.log.Error("budget: list", "err", err)
		return agent, nil
	}
	now := time.Now()
	for _, bud := range budgets {
		if !bud.Active {
			continue
		}
		used, err := b.e.costs.Usage(ctx, UsageQuery{Target: bud.Target, From: budgetWindowStart(bud.Window, now), To: now})
		if err != nil {
			b.e.log.Error("budget: usage", "budget", bud.Name, "err", err)
			continue
		}
		if !budgetExceeded(bud.Limit, used) {
			continue
		}
		switch bud.OnExceed {
		case BudgetDowngrade:
			if agent.FallbackAgentID != nil {
				if fb, ferr := sqlQueryAgentByID(ctx, b.e.db, b.e.prefix, *agent.FallbackAgentID); ferr == nil {
					b.e.log.Info("budget: downgrading to fallback", "budget", bud.Name, "from", agent.Slug, "to", fb.Slug)
					return fb, nil
				}
			}
			// No usable fallback — block rather than silently overspend.
			return agent, budgetExceededErr(bud, used, now)
		case BudgetNotify:
			b.e.log.Info("budget: exceeded (notify)", "budget", bud.Name, "used_usd", used.TotalUSD, "limit_usd", bud.Limit.USD)
		default: // BudgetBlock
			return agent, budgetExceededErr(bud, used, now)
		}
	}
	return agent, nil
}

func budgetExceeded(limit BudgetLimit, used *UsageSummary) bool {
	if limit.USD > 0 && used.TotalUSD >= limit.USD {
		return true
	}
	if limit.Tokens > 0 && used.TotalTokens >= limit.Tokens {
		return true
	}
	if limit.Steps > 0 && used.StepCount >= limit.Steps {
		return true
	}
	return false
}

func budgetExceededErr(bud *Budget, used *UsageSummary, now time.Time) *BudgetExceededError {
	return &BudgetExceededError{
		BudgetName:  bud.Name,
		Window:      string(bud.Window),
		UsedUSD:     used.TotalUSD,
		LimitUSD:    bud.Limit.USD,
		UsedTokens:  used.TotalTokens,
		LimitTokens: bud.Limit.Tokens,
		ResetsAt:    budgetWindowEnd(bud.Window, now),
	}
}

// budgetWindowStart returns the lower bound of a rolling budget window.
func budgetWindowStart(w BudgetWindow, now time.Time) time.Time {
	switch w {
	case BudgetWindowHour:
		return now.Add(-time.Hour)
	case BudgetWindowDay:
		return now.Add(-24 * time.Hour)
	case BudgetWindowWeek:
		return now.Add(-7 * 24 * time.Hour)
	case BudgetWindowMonth:
		return now.Add(-30 * 24 * time.Hour)
	default: // lifetime (or unknown) — count everything
		return time.Time{}
	}
}

func budgetWindowEnd(w BudgetWindow, now time.Time) time.Time {
	switch w {
	case BudgetWindowHour:
		return now.Add(time.Hour)
	case BudgetWindowDay:
		return now.Add(24 * time.Hour)
	case BudgetWindowWeek:
		return now.Add(7 * 24 * time.Hour)
	case BudgetWindowMonth:
		return now.Add(30 * 24 * time.Hour)
	default:
		return time.Time{}
	}
}
