package loom

// budget_test.go — proves budget enforcement actually works. The enforcer was a
// no-op pass-through stub; now RunStep blocks, downgrades to a fallback agent, or
// notifies when a platform's rolling spend exceeds an active budget.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// runWithBudget seeds prior spend of priorUSD for platform "u1", a $1 budget with
// the given action, then runs a step and returns the step, the main/fallback
// agent IDs, and the error.
func runWithBudget(t *testing.T, action BudgetAction, priorUSD float64, withFallback bool) (*Step, uuid.UUID, uuid.UUID, error) {
	t.Helper()
	ctx := context.Background()
	e, _ := reproEngine(t, "budget_"+string(action), map[string]Generator{"g": okGen{}}, PollerConfig{})

	var fbID uuid.UUID
	var fbPtr *uuid.UUID
	if withFallback {
		fb := &Agent{ID: uuid.New(), Slug: "cheap", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}
		if err := e.Agents().Create(ctx, fb); err != nil {
			t.Fatal(err)
		}
		fbID = fb.ID
		fbPtr = &fbID
	}
	main := &Agent{ID: uuid.New(), Slug: "main", Version: 1, Modal: ModalityText, GeneratorSlug: "g", FallbackAgentID: fbPtr}
	if err := e.Agents().Create(ctx, main); err != nil {
		t.Fatal(err)
	}
	if priorUSD > 0 {
		if err := e.Cost().Record(ctx, CostRecord{PlatformID: "u1", USDCost: priorUSD, Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Budgets().Create(ctx, &Budget{
		Name:     "cap",
		Target:   BudgetTarget{Kind: TargetPlatformID, Key: "u1"},
		Window:   BudgetWindowDay,
		Limit:    BudgetLimit{USD: 1.0},
		OnExceed: action,
		Active:   true,
	}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "u1", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "main", AgentVersion: 1})
	return step, main.ID, fbID, err
}

func TestBudget_BlockWhenExceeded(t *testing.T) {
	_, _, _, err := runWithBudget(t, BudgetBlock, 5.0, false)
	var be *BudgetExceededError
	if !errors.As(err, &be) {
		t.Fatalf("want *BudgetExceededError, got %v", err)
	}
}

func TestBudget_DowngradeToFallback(t *testing.T) {
	step, _, fbID, err := runWithBudget(t, BudgetDowngrade, 5.0, true)
	if err != nil {
		t.Fatalf("downgrade should run, got: %v", err)
	}
	if step.AgentID != fbID {
		t.Errorf("step ran under %s, want fallback %s", step.AgentID, fbID)
	}
}

func TestBudget_DowngradeWithoutFallbackBlocks(t *testing.T) {
	// Downgrade is configured but the over-budget agent has no fallback: it must
	// block (money safety), not silently proceed on the main agent.
	step, mainID, _, err := runWithBudget(t, BudgetDowngrade, 5.0, false)
	var be *BudgetExceededError
	if !errors.As(err, &be) {
		t.Fatalf("downgrade with no fallback agent must block, got step=%v err=%v (mainID=%s)", step, err, mainID)
	}
}

func TestBudget_NotifyStillRunsOnMainAgent(t *testing.T) {
	step, mainID, _, err := runWithBudget(t, BudgetNotify, 5.0, false)
	if err != nil {
		t.Fatalf("notify should run, got: %v", err)
	}
	if step.AgentID != mainID {
		t.Errorf("notify must not change the agent: ran under %s, want %s", step.AgentID, mainID)
	}
}

func TestBudget_UnderLimitRunsNormally(t *testing.T) {
	step, mainID, _, err := runWithBudget(t, BudgetBlock, 0.5, false)
	if err != nil {
		t.Fatalf("under budget should run, got: %v", err)
	}
	if step.AgentID != mainID {
		t.Errorf("under budget ran under %s, want %s", step.AgentID, mainID)
	}
}

func TestBudget_StepLimitBlocks(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "budget_steps", map[string]Generator{"g": okGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{ID: uuid.New(), Slug: "main", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // two prior steps -> StepCount 2
		if err := e.Cost().Record(ctx, CostRecord{PlatformID: "u1", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Budgets().Create(ctx, &Budget{Name: "steps", Target: BudgetTarget{Kind: TargetPlatformID, Key: "u1"}, Window: BudgetWindowDay, Limit: BudgetLimit{Steps: 2}, OnExceed: BudgetBlock, Active: true}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "u1", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	_, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "main", AgentVersion: 1})
	var be *BudgetExceededError
	if !errors.As(err, &be) {
		t.Fatalf("step-limit budget should block, got %v", err)
	}
}

func TestBudget_CreateRejectsUnenforcedTarget(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "budget_reject", map[string]Generator{"g": okGen{}}, PollerConfig{})
	err := e.Budgets().Create(ctx, &Budget{
		Name: "sess", Target: BudgetTarget{Kind: "session", Key: "x"},
		Window: BudgetWindowDay, Limit: BudgetLimit{USD: 1}, OnExceed: BudgetBlock, Active: true,
	})
	if err == nil {
		t.Fatal("Create should reject a non-platform_id budget that would never be enforced")
	}
}
