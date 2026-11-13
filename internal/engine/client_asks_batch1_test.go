package engine

// client_asks_batch1_test.go: the small, unambiguous items from the client's
// asks doc (structured output between agents, typed refusals, step budgets).

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type rejectingGen struct{}

func (rejectingGen) Modality() Modality { return ModalityText }
func (rejectingGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return nil, &GenerationError{Kind: GenerationRejected, Message: "refused (cyber)"}
}

type failedResultSyncGen struct{}

func (failedResultSyncGen) Modality() Modality { return ModalityText }
func (failedResultSyncGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return NewFailedResult(ModalityText, errors.New("provider said no")), nil
}

func runOneStep(t *testing.T, gen Generator) error {
	t.Helper()
	ctx := context.Background()
	e, _ := reproEngine(t, "batch1", map[string]Generator{"g": gen}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	_, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
	return err
}

// A sync adapter's own GenerationError keeps its Kind instead of being
// re-wrapped as a transport failure.
func TestSyncGenerationErrorKeepsKind(t *testing.T) {
	var ge *GenerationError
	if err := runOneStep(t, rejectingGen{}); !errors.As(err, &ge) || ge.Kind != GenerationRejected || ge.Provider != "g" {
		t.Fatalf("err = %v, want GenerationRejected from provider g", err)
	}
}

// A failed result from a sync Generate is a rejection, as on the streaming
// path, not a step to persist.
func TestSyncFailedResultIsRejected(t *testing.T) {
	var ge *GenerationError
	if err := runOneStep(t, failedResultSyncGen{}); !errors.As(err, &ge) || ge.Kind != GenerationRejected || ge.Message != "provider said no" {
		t.Fatalf("err = %v, want GenerationRejected with the provider message", err)
	}
}

func TestResultTextOfStructured(t *testing.T) {
	if got := ResultText(NewStructuredResult(map[string]any{"text": "hi", "x": 1}, 0, 0)); got != "hi" {
		t.Fatalf("text field: got %q", got)
	}
	if got := ResultText(NewStructuredResult(map[string]any{"hp": 9}, 0, 0)); got != `{"hp":9}` {
		t.Fatalf("no text field: got %q, want the JSON encoding", got)
	}
}

func TestBudgetErrorNamesTheLimitThatTripped(t *testing.T) {
	e := &BudgetExceededError{BudgetName: "table", Window: "day", UsedSteps: 50, LimitSteps: 50}
	if msg := e.Error(); !strings.Contains(msg, "used 50 of 50 steps") {
		t.Fatalf("Error() = %q", msg)
	}
}

// A budget on one session caps that game only, whoever acts in it.
func TestSessionBudget(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "sessbudget", map[string]Generator{"g": okGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	gameA := &Session{PlatformID: "host-key", State: State{Modality: ModalityText}}
	gameB := &Session{PlatformID: "host-key", State: State{Modality: ModalityText}}
	for _, s := range []*Session{gameA, gameB} {
		if err := e.Sessions().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Budgets().Create(ctx, &Budget{Name: "game-a", Target: BudgetTarget{Kind: TargetSessionID, Key: gameA.ID.String()},
		Window: BudgetWindowLifetime, Limit: BudgetLimit{Steps: 1}, OnExceed: BudgetBlock, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.Budgets().Create(ctx, &Budget{Name: "bad", Target: BudgetTarget{Kind: TargetSessionID, Key: "not-a-uuid"}}); err == nil {
		t.Fatal("session budget with a non-UUID key accepted")
	}
	if _, err := e.RunStep(ctx, gameA, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}
	var be *BudgetExceededError
	if _, err := e.RunStep(ctx, gameA, StepRequest{AgentSlug: "a"}); !errors.As(err, &be) || be.BudgetName != "game-a" {
		t.Fatalf("game A second step: err = %v, want game-a budget exceeded", err)
	}
	for range 2 {
		if _, err := e.RunStep(ctx, gameB, StepRequest{AgentSlug: "a"}); err != nil {
			t.Fatalf("game B blocked by game A's budget: %v", err)
		}
	}
}

func TestEstimateUsesConfiguredPricing(t *testing.T) {
	ctx := context.Background()
	e := pricedEngine(t, "estimate", map[string]Generator{"default": okGen{}, "meshy-3d": asyncModel3DGen{}},
		map[string]ModelPrice{"claude-opus-5-5": {InputPer1M: 4, OutputPer1M: 20}})
	e.jobPricing = map[string]float64{"meshy-3d": 0.4}
	if err := e.Agents().Create(ctx, &Agent{Slug: "forge", Version: 1, Modal: ModalityModel3D, GeneratorSlug: "meshy-3d"}); err != nil {
		t.Fatal(err)
	}
	forge, _ := e.Agents().Get(ctx, "", "forge", 1)

	est, err := e.Cost().Estimate(ctx, EstimateRequest{Model: "claude-opus-5-5", InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if est.USDLow != 4 || est.USDHigh != 24 || !est.PricingConfigured {
		t.Fatalf("token estimate = %+v, want 4 / 24 configured", est)
	}
	if est, _ = e.Cost().Estimate(ctx, EstimateRequest{AgentID: forge.ID}); est.USDHigh != 0.4 || !est.PricingConfigured || est.Model != "meshy-3d" {
		t.Fatalf("job estimate = %+v, want 0.40 from JobPricing", est)
	}
	if est, _ = e.Cost().Estimate(ctx, EstimateRequest{Model: "unknown", InputTokens: 10}); est.PricingConfigured {
		t.Fatalf("unpriced model reported as configured: %+v", est)
	}
}

func TestVerifyGeneratorRouting(t *testing.T) {
	e, _ := reproEngine(t, "verify", map[string]Generator{"g": okGen{}}, PollerConfig{})
	if _, err := e.VerifyGenerator(context.Background(), "g", nil); !errors.Is(err, ErrVerifyNotSupported) {
		t.Fatalf("err = %v, want ErrVerifyNotSupported", err)
	}
	if _, err := e.VerifyGenerator(context.Background(), "nope", nil); !errors.Is(err, ErrGeneratorNotRegistered) {
		t.Fatalf("err = %v, want ErrGeneratorNotRegistered", err)
	}
}
