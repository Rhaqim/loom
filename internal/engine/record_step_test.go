package engine

// record_step_test.go: human-authored steps (client ask §3). A human game
// master's narration is recorded as a real step — persisted, checkpointed,
// hooks run, attributed to its author — with no generator call and no cost row.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// recordCountGen fails the test's expectations if a recorded step ever calls it.
type recordCountGen struct{ calls *atomic.Int32 }

func (recordCountGen) Modality() Modality { return ModalityText }
func (g recordCountGen) Generate(_ context.Context, req GenerateRequest) (Result, error) {
	g.calls.Add(1)
	return NewTextResult("echo: "+req.UserPrompt, "stop", 3, 3), nil
}

func recordSetup(t *testing.T) (*Engine, *Session, *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	var calls atomic.Int32
	e, _ := reproEngine(t, "record", map[string]Generator{"g": recordCountGen{&calls}}, PollerConfig{})
	ut := &Prompt{Slug: "ut", Version: 1, Kind: PromptKindUserTemplate, Body: "narration: {{.Inputs.narration}}"}
	if err := e.Prompts().Create(ctx, ut); err != nil {
		t.Fatal(err)
	}
	if err := e.Agents().Create(ctx, &Agent{Slug: "human-dm", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Agents().Create(ctx, &Agent{Slug: "referee", Version: 1, Modal: ModalityText, GeneratorSlug: "g", UserTemplateID: ut.ID}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "game:abc", State: State{Modality: ModalityText, Vars: map[string]any{}}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return e, sess, &calls
}

func TestRecordStepPersistsWithoutGenerating(t *testing.T) {
	ctx := context.Background()
	e, sess, calls := recordSetup(t)
	// A post-hook sees the recorded step and updates state from it.
	e.Hooks().RegisterPost("tally", func(_ context.Context, req *StepRequest, res Result) (Result, error) {
		if req.Recorded() && req.Author() == "participant:host" {
			req.Session.State.Vars["last_narrator"] = req.Author()
		}
		return res, nil
	})

	step, err := e.RecordStep(ctx, sess, ExternalStep{
		AgentSlug: "human-dm", Author: "participant:host",
		Result: NewTextResult("The gate groans open.", "stop", 0, 0),
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "open the gate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("generator called %d times for a recorded step", calls.Load())
	}
	if step.Author != "participant:host" || ResultText(step.Result) != "The gate groans open." {
		t.Fatalf("step = author %q text %q", step.Author, ResultText(step.Result))
	}
	if sess.State.Vars["last_narrator"] != "participant:host" {
		t.Fatalf("post-hook state write lost: %v", sess.State.Vars)
	}
	rows, err := e.Cost().ByStep(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("recorded step wrote %d cost rows, want 0", len(rows))
	}

	// Persisted with its author, and checkpointed.
	got, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 || got.History[0].Author != "participant:host" {
		t.Fatalf("reloaded history = %+v", got.History)
	}
	st, _, err := e.Sessions().StateAt(ctx, sess.ID, step.Index)
	if err != nil || st.Vars["last_narrator"] != "participant:host" {
		t.Fatalf("checkpoint = %v, %v", st, err)
	}
}

func TestRecordStepRejectsWhatItCannotRecord(t *testing.T) {
	ctx := context.Background()
	e, sess, _ := recordSetup(t)
	if _, err := e.RecordStep(ctx, sess, ExternalStep{AgentSlug: "human-dm", Result: NewTextResult("x", "stop", 0, 0)}); err == nil {
		t.Error("missing Author accepted")
	}
	pending := NewPendingResult(ModalityText, &TaskHandle{Handle: "job"})
	if _, err := e.RecordStep(ctx, sess, ExternalStep{AgentSlug: "human-dm", Author: "a", Result: pending}); err == nil {
		t.Error("pending result accepted")
	}
	e.Hooks().RegisterPost("picky", func(_ context.Context, req *StepRequest, res Result) (Result, error) {
		return nil, ErrRetryWith(RetryAnnotation{Reason: "too short"})
	})
	_, err := e.RecordStep(ctx, sess, ExternalStep{AgentSlug: "human-dm", Author: "a", Result: NewTextResult("x", "stop", 0, 0)})
	if err == nil || !strings.Contains(err.Error(), "no generator to re-run") {
		t.Errorf("retry from a post-hook: err = %v", err)
	}
}

// A budget that blocks generation does not block a human's narration: it
// costs nothing.
func TestRecordStepIgnoresBudgets(t *testing.T) {
	ctx := context.Background()
	e, sess, _ := recordSetup(t)
	if err := e.Budgets().Create(ctx, &Budget{Name: "cap", Target: BudgetTarget{Kind: TargetPlatformID, Key: "game:abc"},
		Window: BudgetWindowDay, Limit: BudgetLimit{Steps: 1}, OnExceed: BudgetBlock, Active: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "referee"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "referee"}); err == nil {
		t.Fatal("budget did not block the second generated step")
	}
	if _, err := e.RecordStep(ctx, sess, ExternalStep{AgentSlug: "human-dm", Author: "participant:host",
		Result: NewTextResult("Still free.", "stop", 0, 0)}); err != nil {
		t.Fatalf("budget blocked a recorded step: %v", err)
	}
}

func TestHumanLedTurn(t *testing.T) {
	ctx := context.Background()
	e, sess, calls := recordSetup(t)
	turn, err := e.RunTurn(ctx, sess, TurnRequest{
		Flow: Flow{Slug: "t",
			Lead:      FlowAgent{AgentSlug: "human-dm", OutputKey: "narration"},
			Followers: []FlowAgent{{AgentSlug: "referee"}},
		},
		LeadResult: NewTextResult("A wolf leaps.", "stop", 0, 0),
		LeadAuthor: "participant:host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Lead.Author != "participant:host" {
		t.Fatalf("lead author = %q", turn.Lead.Author)
	}
	if calls.Load() != 1 {
		t.Fatalf("generator calls = %d, want 1 (the referee only)", calls.Load())
	}
	ref := turn.Followers["referee"]
	if ref == nil || ResultText(ref.Result) != "echo: narration: A wolf leaps." {
		t.Fatalf("referee did not consume the human narration: %+v", ref)
	}
	rows, _ := e.Cost().ByStep(ctx, sess.ID)
	if len(rows) != 1 || rows[0].StepID != ref.ID {
		t.Fatalf("cost rows = %+v, want only the referee's", rows)
	}
	if _, err := e.RunTurn(ctx, sess, TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "human-dm"}},
		LeadResult: NewTextResult("x", "stop", 0, 0)}); err == nil {
		t.Fatal("LeadResult without LeadAuthor accepted")
	}
}
