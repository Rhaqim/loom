package engine

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// BudgetManager is the public interface returned by Engine.Budgets().
type BudgetManager interface {
	// Create persists a new budget.
	Create(ctx context.Context, b *Budget) error
	// Get retrieves a budget by ID.
	Get(ctx context.Context, id uuid.UUID) (*Budget, error)
	// List returns all budgets matching the given target.
	List(ctx context.Context, targetKind, targetKey string) ([]*Budget, error)
	// Delete removes a budget.
	Delete(ctx context.Context, id uuid.UUID) error
}

// Budget defines a (target, window, limit) spending triple.
type Budget struct {
	ID          uuid.UUID
	Name        string
	Target      BudgetTarget
	Window      BudgetWindow
	Limit       BudgetLimit
	OnExceed    BudgetAction
	TagsInclude []string
	TagsExclude []string
	Active      bool
	CreatedAt   time.Time
}

// BudgetTarget identifies the entity a budget applies to. Only Kind ==
// TargetPlatformID is currently enforced; Create rejects other kinds.
type BudgetTarget struct {
	Kind string // currently only "platform_id" is enforced
	Key  string
}

// BudgetWindow is the rolling time window for a budget.
type BudgetWindow string

const (
	BudgetWindowHour     BudgetWindow = "hour"
	BudgetWindowDay      BudgetWindow = "day"
	BudgetWindowWeek     BudgetWindow = "week"
	BudgetWindowMonth    BudgetWindow = "month"
	BudgetWindowLifetime BudgetWindow = "lifetime"
)

// BudgetLimit constrains usage by USD, tokens, and/or step count.
type BudgetLimit struct {
	USD    float64 // 0 = no USD limit
	Tokens int     // 0 = no token limit
	Steps  int     // 0 = no step limit
}

// BudgetAction is what happens when a budget is exceeded.
type BudgetAction string

const (
	BudgetBlock     BudgetAction = "block"
	BudgetDowngrade BudgetAction = "downgrade"
	BudgetNotify    BudgetAction = "notify"
)

// TargetPlatformID is the BudgetTarget.Kind for per-user budgets.
const TargetPlatformID = "platform_id"
