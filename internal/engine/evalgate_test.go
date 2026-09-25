package engine

// evalgate_test.go — the EXPERIMENTAL evaluation subsystem. The load-bearing
// property under test is that it is OPT-IN: an engine with no Config.Evaluator
// must behave exactly as it did before, and every gate must be inert.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rhaqim/loom/evaluator"
	"github.com/rhaqim/loom/evaluator/stub"
)

func evalSession(t *testing.T, e *Engine) *Session {
	t.Helper()
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

// --- opt-in ---

func TestEvaluateWithoutAnEvaluatorIsADistinctError(t *testing.T) {
	// "The feature is off" must be distinguishable from "the evaluation
	// failed", so a caller can fall back to its pre-evaluator behaviour.
	e, _ := reproEngine(t, "evaloff", map[string]Generator{"g": okGen{}}, PollerConfig{})
	if _, err := e.Evaluate(context.Background(), "s",
		map[string]EvalQuestion{"a": EvalNoul("q")}); !errors.Is(err, ErrEvaluatorNotConfigured) {
		t.Fatalf("Evaluate() = %v, want ErrEvaluatorNotConfigured", err)
	}
	if e.Evaluator() != nil {
		t.Error("Evaluator() = non-nil on an engine that never configured one")
	}
}

func TestEvalGateIsInertWithoutAnEvaluator(t *testing.T) {
	// Registering the hook on an engine with no evaluator must not change the
	// step's outcome, so code can register it unconditionally.
	ctx := context.Background()
	e, _ := reproEngine(t, "gateinert", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	var asked bool
	e.Hooks().RegisterPost("gate", e.EvalGate(EvalGateConfig{
		Questions: func(*StepRequest, Result) map[string]EvalQuestion {
			asked = true
			return map[string]EvalQuestion{"x": EvalNoul("q")}
		},
		Decide: func(EvalAnswers) EvalDecision {
			return EvalDecision{Verdict: EvalReject, Reason: "should never run"}
		},
	}))

	step, err := e.RunStep(ctx, evalSession(t, e), StepRequest{
		AgentSlug: "a",
		Action:    &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	})
	if err != nil {
		t.Fatalf("RunStep() = %v, want the gate to be inert", err)
	}
	if ResultText(step.Result) != "ok" {
		t.Errorf("result = %q, want the generator's output untouched", ResultText(step.Result))
	}
	if asked {
		t.Error("the gate built questions with no evaluator configured; it should short-circuit first")
	}
}

func TestSetEvaluatorTurnsTheSubsystemOnAtRuntime(t *testing.T) {
	e, _ := reproEngine(t, "evalset", map[string]Generator{"g": okGen{}}, PollerConfig{})
	e.SetEvaluator(stub.New().WithNoul("a", 0.9))

	eval, err := e.Evaluate(context.Background(), "s", map[string]EvalQuestion{"a": EvalNoul("q")})
	if err != nil {
		t.Fatal(err)
	}
	if !eval.Answers.Yes("a", 0.8) {
		t.Errorf("answer = %v, want the stub's 0.9", eval.Answers.Noul("a"))
	}

	// And back off again.
	e.SetEvaluator(nil)
	if _, err := e.Evaluate(context.Background(), "s",
		map[string]EvalQuestion{"a": EvalNoul("q")}); !errors.Is(err, ErrEvaluatorNotConfigured) {
		t.Fatalf("after SetEvaluator(nil), Evaluate() = %v, want ErrEvaluatorNotConfigured", err)
	}
}

// --- gate verdicts ---

func TestEvalGateRetryCarriesTheReasonToTheGenerator(t *testing.T) {
	ctx := context.Background()
	ev := stub.New().WithNoul("repeats", 0.9)
	gen := &annotationGen{}
	e, _ := reproEngine(t, "gateretry", map[string]Generator{"g": gen}, PollerConfig{})
	e.SetEvaluator(ev)
	mustAgent(t, e, "a", "g")

	var attempts int
	e.Hooks().RegisterPost("gate", e.EvalGate(EvalGateConfig{
		Agents: []string{"a"},
		Questions: func(*StepRequest, Result) map[string]EvalQuestion {
			attempts++
			return map[string]EvalQuestion{"repeats": EvalNoul("Does it repeat?")}
		},
		Decide: func(a EvalAnswers) EvalDecision {
			// Pass on the second attempt so the step can settle.
			if attempts > 1 {
				return EvalDecision{Verdict: EvalAccept}
			}
			if a.Yes("repeats", 0.7) {
				return EvalDecision{
					Verdict:   EvalRetry,
					Reason:    "this scene repeats the previous one; advance the story",
					Forbidden: []string{"the same alley"},
				}
			}
			return EvalDecision{Verdict: EvalAccept}
		},
	}))

	if _, err := e.RunStep(ctx, evalSession(t, e), StepRequest{
		AgentSlug: "a",
		Action:    &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	}); err != nil {
		t.Fatal(err)
	}

	anns := gen.annotations()
	if len(anns) != 2 {
		t.Fatalf("generator saw %d attempts, want 2 (the original plus the gated retry)", len(anns))
	}
	if len(anns[1]) != 1 {
		t.Fatalf("retry attempt carried %d annotations, want 1", len(anns[1]))
	}
	if !strings.Contains(anns[1][0].Reason, "repeats the previous one") {
		t.Errorf("retry reason = %q, want the gate's reason", anns[1][0].Reason)
	}
	if len(anns[1][0].Forbidden) != 1 || anns[1][0].Forbidden[0] != "the same alley" {
		t.Errorf("Forbidden = %v, want the gate's list", anns[1][0].Forbidden)
	}
}

func TestEvalGateRejectFailsTheStep(t *testing.T) {
	e, _ := reproEngine(t, "gatereject", map[string]Generator{"g": okGen{}}, PollerConfig{})
	e.SetEvaluator(stub.New().WithNoul("unsafe", 0.99))
	mustAgent(t, e, "a", "g")

	e.Hooks().RegisterPost("gate", e.EvalGate(EvalGateConfig{
		Questions: func(*StepRequest, Result) map[string]EvalQuestion {
			return map[string]EvalQuestion{"unsafe": EvalNoul("Is it unsafe?")}
		},
		Decide: func(a EvalAnswers) EvalDecision {
			if a.Yes("unsafe", 0.9) {
				return EvalDecision{Verdict: EvalReject, Reason: "unsafe content"}
			}
			return EvalDecision{Verdict: EvalAccept}
		},
	}))

	_, err := e.RunStep(context.Background(), evalSession(t, e), StepRequest{
		AgentSlug: "a",
		Action:    &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe content") {
		t.Fatalf("RunStep() = %v, want the step to fail with the gate's reason", err)
	}
}

func TestEvalGateSkipsUnnamedAgents(t *testing.T) {
	ctx := context.Background()
	ev := stub.New()
	e, _ := reproEngine(t, "gatescope", map[string]Generator{"g": okGen{}}, PollerConfig{})
	e.SetEvaluator(ev)
	mustAgent(t, e, "author", "g")
	mustAgent(t, e, "extractor", "g")

	e.Hooks().RegisterPost("gate", e.EvalGate(EvalGateConfig{
		Agents: []string{"author"},
		Questions: func(*StepRequest, Result) map[string]EvalQuestion {
			return map[string]EvalQuestion{"x": EvalNoul("q")}
		},
		Decide: func(EvalAnswers) EvalDecision { return EvalDecision{Verdict: EvalAccept} },
	}))

	sess := evalSession(t, e)
	// A fresh Action per step: an Action carries its own id, so reusing one
	// pointer across two steps collides on persist.
	act := func() *Action {
		return &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}}
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "extractor", Action: act()}); err != nil {
		t.Fatal(err)
	}
	if n := len(ev.Calls()); n != 0 {
		t.Fatalf("evaluated %d times for an unlisted agent, want 0", n)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "author", Action: act()}); err != nil {
		t.Fatal(err)
	}
	if n := len(ev.Calls()); n != 1 {
		t.Fatalf("evaluated %d times for the listed agent, want 1", n)
	}
}

func TestEvalGateSkipsWhenQuestionsAreEmpty(t *testing.T) {
	// Returning no questions must cost nothing — that is how a gate opts out of
	// a turn it cannot judge (a first scene with no previous one to compare).
	ev := stub.New()
	e, _ := reproEngine(t, "gatenoq", map[string]Generator{"g": okGen{}}, PollerConfig{})
	e.SetEvaluator(ev)
	mustAgent(t, e, "a", "g")

	e.Hooks().RegisterPost("gate", e.EvalGate(EvalGateConfig{
		Questions: func(*StepRequest, Result) map[string]EvalQuestion { return nil },
		Decide: func(EvalAnswers) EvalDecision {
			t.Error("Decide ran with no questions asked")
			return EvalDecision{}
		},
	}))

	if _, err := e.RunStep(context.Background(), evalSession(t, e), StepRequest{
		AgentSlug: "a",
		Action:    &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(ev.Calls()); n != 0 {
		t.Errorf("made %d evaluations for an empty question set, want 0", n)
	}
}

func TestEvalGateFailsOpenByDefaultAndClosedOnRequest(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("evaluator down")
	act := func() *Action {
		return &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}}
	}
	cfg := func(failClosed bool) EvalGateConfig {
		return EvalGateConfig{
			FailClosed: failClosed,
			Questions: func(*StepRequest, Result) map[string]EvalQuestion {
				return map[string]EvalQuestion{"x": EvalNoul("q")}
			},
			Decide: func(EvalAnswers) EvalDecision { return EvalDecision{Verdict: EvalAccept} },
		}
	}

	// Default: a decision-layer outage degrades quality, it does not take the
	// product down.
	open, _ := reproEngine(t, "gateopen", map[string]Generator{"g": okGen{}}, PollerConfig{})
	open.SetEvaluator(stub.New().WithError(boom))
	mustAgent(t, open, "a", "g")
	open.Hooks().RegisterPost("gate", open.EvalGate(cfg(false)))
	if _, err := open.RunStep(ctx, evalSession(t, open), StepRequest{AgentSlug: "a", Action: act()}); err != nil {
		t.Fatalf("RunStep() = %v, want the default gate to fail open", err)
	}

	closed, _ := reproEngine(t, "gateclosed", map[string]Generator{"g": okGen{}}, PollerConfig{})
	closed.SetEvaluator(stub.New().WithError(boom))
	mustAgent(t, closed, "a", "g")
	closed.Hooks().RegisterPost("gate", closed.EvalGate(cfg(true)))
	if _, err := closed.RunStep(ctx, evalSession(t, closed), StepRequest{AgentSlug: "a", Action: act()}); !errors.Is(err, boom) {
		t.Fatalf("RunStep() = %v, want FailClosed to surface the evaluator error", err)
	}
}

func TestEvalGateWithoutRequiredFuncsFailsLoudly(t *testing.T) {
	// A misconfigured gate that silently accepted everything would look
	// registered and do nothing — the worst failure mode for a safety check.
	e, _ := reproEngine(t, "gatemisconf", map[string]Generator{"g": okGen{}}, PollerConfig{})
	e.SetEvaluator(stub.New())
	mustAgent(t, e, "a", "g")
	e.Hooks().RegisterPost("gate", e.EvalGate(EvalGateConfig{}))

	_, err := e.RunStep(context.Background(), evalSession(t, e), StepRequest{
		AgentSlug: "a",
		Action:    &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "requires both Questions and Decide") {
		t.Fatalf("RunStep() = %v, want a misconfiguration error", err)
	}
}

func TestEvalGateStateDefaultsToTheResultText(t *testing.T) {
	ev := stub.New()
	e, _ := reproEngine(t, "gatestate", map[string]Generator{"g": okGen{}}, PollerConfig{})
	e.SetEvaluator(ev)
	mustAgent(t, e, "a", "g")
	e.Hooks().RegisterPost("gate", e.EvalGate(EvalGateConfig{
		Questions: func(*StepRequest, Result) map[string]EvalQuestion {
			return map[string]EvalQuestion{"x": EvalNoul("q")}
		},
		Decide: func(EvalAnswers) EvalDecision { return EvalDecision{Verdict: EvalAccept} },
	}))

	if _, err := e.RunStep(context.Background(), evalSession(t, e), StepRequest{
		AgentSlug: "a",
		Action:    &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	calls := ev.Calls()
	if len(calls) != 1 || calls[0].State != "ok" {
		t.Fatalf("evaluated state = %#v, want the result text \"ok\"", calls[0].State)
	}
}

func TestEvalGateOnEvaluationSeesUsage(t *testing.T) {
	// Cost accounting has to be possible, or the "it's cheap" claim is unaudited.
	e, _ := reproEngine(t, "gateusage", map[string]Generator{"g": okGen{}}, PollerConfig{})
	e.SetEvaluator(stub.New())
	mustAgent(t, e, "a", "g")

	var seen int
	e.Hooks().RegisterPost("gate", e.EvalGate(EvalGateConfig{
		Questions: func(*StepRequest, Result) map[string]EvalQuestion {
			return map[string]EvalQuestion{"x": EvalNoul("q")}
		},
		Decide:       func(EvalAnswers) EvalDecision { return EvalDecision{Verdict: EvalAccept} },
		OnEvaluation: func(_ *StepRequest, ev Evaluation) { seen++; _ = ev.Usage },
	}))

	if _, err := e.RunStep(context.Background(), evalSession(t, e), StepRequest{
		AgentSlug: "a",
		Action:    &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("OnEvaluation fired %d times, want 1", seen)
	}
}

// --- flow follower gate ---

func TestFollowerGateSkipsWithoutRunningTheAgent(t *testing.T) {
	ctx := context.Background()
	gen := &countingGen{}
	e, _ := reproEngine(t, "flowgate", map[string]Generator{"g": gen}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	mustAgent(t, e, "kept", "g")
	mustAgent(t, e, "skipped", "g")

	turn, err := e.RunTurn(ctx, evalSession(t, e), TurnRequest{
		Flow: Flow{
			Slug: "t",
			Lead: FlowAgent{AgentSlug: "lead"},
			Followers: []FlowAgent{
				{AgentSlug: "kept"},
				{AgentSlug: "skipped", When: func(context.Context, FollowerGate) (bool, error) {
					return false, nil
				}},
			},
		},
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Skipped) != 1 || turn.Skipped[0] != "skipped" {
		t.Errorf("Skipped = %v, want [skipped]", turn.Skipped)
	}
	if _, ok := turn.Followers["skipped"]; ok {
		t.Error("a skipped follower produced a step")
	}
	if _, ok := turn.Errors["skipped"]; ok {
		t.Error("a skip was recorded as an error; it is neither a success nor a failure")
	}
	if _, ok := turn.Followers["kept"]; !ok {
		t.Error("the ungated follower did not run")
	}
	// The whole point is the saved generator call: lead + kept, not + skipped.
	if n := gen.count(); n != 2 {
		t.Errorf("generator called %d times, want 2 (lead and the ungated follower)", n)
	}
}

func TestFollowerGateSeesTheLeadOutput(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "flowgatein", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	mustAgent(t, e, "f", "g")

	var gotLead, gotInput string
	var gotSession bool
	if _, err := e.RunTurn(ctx, evalSession(t, e), TurnRequest{
		Flow: Flow{
			Slug: "t",
			Lead: FlowAgent{AgentSlug: "lead", OutputKey: "Prose"},
			Followers: []FlowAgent{{AgentSlug: "f", When: func(_ context.Context, g FollowerGate) (bool, error) {
				gotLead = ResultText(g.Lead.Result)
				gotInput, _ = g.Inputs["Prose"].(string)
				gotSession = g.Session != nil
				return true, nil
			}}},
		},
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if gotLead != "ok" {
		t.Errorf("gate saw lead output %q, want the settled lead result", gotLead)
	}
	if gotInput != "ok" {
		t.Errorf("gate saw Inputs[Prose] = %q, want the lead output under its OutputKey", gotInput)
	}
	if !gotSession {
		t.Error("gate saw a nil Session")
	}
}

func TestFollowerGateErrorSkipsAndReports(t *testing.T) {
	ctx := context.Background()
	gen := &countingGen{}
	e, _ := reproEngine(t, "flowgateerr", map[string]Generator{"g": gen}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	mustAgent(t, e, "f", "g")

	var role string
	var gotErr error
	turn, err := e.RunTurn(ctx, evalSession(t, e), TurnRequest{
		Flow: Flow{
			Slug: "t",
			Lead: FlowAgent{AgentSlug: "lead"},
			Followers: []FlowAgent{{AgentSlug: "f", When: func(context.Context, FollowerGate) (bool, error) {
				return true, errors.New("gate broke")
			}}},
		},
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
		OnStep: func(r string, _ *Step, e error) {
			if e != nil {
				role, gotErr = r, e
			}
		},
	})
	// A broken gate must degrade to a missing follower, not a failed turn.
	if err != nil {
		t.Fatalf("RunTurn() = %v, want the turn to survive a gate error", err)
	}
	if turn.Errors["f"] == nil {
		t.Error("Turn.Errors has no entry for the failed gate")
	}
	if len(turn.Skipped) != 1 {
		t.Errorf("Skipped = %v, want the follower skipped", turn.Skipped)
	}
	if role != "follower:f" || gotErr == nil {
		t.Errorf("OnStep fired as (%q, %v), want the follower role and the gate error", role, gotErr)
	}
	// The gate failed before the agent ran, so only the lead reached the generator.
	if n := gen.count(); n != 1 {
		t.Errorf("generator called %d times, want 1", n)
	}
}

func TestFollowerGateBackedByAnEvaluator(t *testing.T) {
	// The end-to-end shape the subsystem exists for: one cheap classification
	// deciding whether a full generation happens.
	ctx := context.Background()
	gen := &countingGen{}
	e, _ := reproEngine(t, "flowgateeval", map[string]Generator{"g": gen}, PollerConfig{})
	e.SetEvaluator(stub.New().WithNoul("needs_art", 0.05))
	mustAgent(t, e, "author", "g")
	mustAgent(t, e, "sensory", "g")

	turn, err := e.RunTurn(ctx, evalSession(t, e), TurnRequest{
		Flow: Flow{
			Slug: "t",
			Lead: FlowAgent{AgentSlug: "author", OutputKey: "Prose"},
			Followers: []FlowAgent{{AgentSlug: "sensory", When: func(ctx context.Context, g FollowerGate) (bool, error) {
				eval, err := e.Evaluate(ctx, ResultText(g.Lead.Result), map[string]EvalQuestion{
					"needs_art": EvalNoul("Does this scene introduce new imagery worth illustrating?"),
				})
				if err != nil {
					// Fail open: run the follower if we cannot decide.
					return true, nil
				}
				return eval.Answers.Yes("needs_art", 0.5), nil
			}}},
		},
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Skipped) != 1 || turn.Skipped[0] != "sensory" {
		t.Errorf("Skipped = %v, want the sensory follower skipped on a 0.05 answer", turn.Skipped)
	}
	if n := gen.count(); n != 1 {
		t.Errorf("generator called %d times, want 1 — the skipped follower is the saving", n)
	}
}

// --- helpers ---

// annotationGen records the retry annotations each attempt was given.
type annotationGen struct {
	mu   sync.Mutex
	anns [][]RetryAnnotation
}

func (g *annotationGen) Modality() Modality { return ModalityText }
func (g *annotationGen) Generate(_ context.Context, req GenerateRequest) (Result, error) {
	g.mu.Lock()
	g.anns = append(g.anns, req.Annotations)
	g.mu.Unlock()
	return NewTextResult("draft", "stop", 1, 1), nil
}
func (g *annotationGen) annotations() [][]RetryAnnotation {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]RetryAnnotation(nil), g.anns...)
}

// countingGen counts how many times a generator was actually invoked, which is
// the unit a follower gate saves.
type countingGen struct {
	mu sync.Mutex
	n  int
}

func (g *countingGen) Modality() Modality { return ModalityText }
func (g *countingGen) Generate(context.Context, GenerateRequest) (Result, error) {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
	return NewTextResult("ok", "stop", 1, 1), nil
}
func (g *countingGen) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

// The engine's alias types must be the evaluator package's own, so a value
// built with either spelling is interchangeable.
var (
	_ Evaluator    = (evaluator.Evaluator)(nil)
	_ EvalQuestion = evaluator.Question{}
	_ EvalAnswers  = evaluator.Answers{}
)
