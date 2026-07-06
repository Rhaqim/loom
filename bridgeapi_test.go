package loom

// bridgeapi_test.go — proves L3 (OnStreamEnd), L4 (hook attempt/role/annotation
// visibility), L5 (PromptRef.Literal), and L6 (per-FlowAgent retry mode).

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// kbStreamGen streams "stream-N" for GenerateStream and returns "sync-N" for
// Generate, sharing one call counter so a test can tell which path ran.
type kbStreamGen struct {
	mu    sync.Mutex
	calls int
}

func (g *kbStreamGen) Modality() Modality { return ModalityText }

func (g *kbStreamGen) next() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.calls
}

func (g *kbStreamGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return NewTextResult(fmt.Sprintf("sync-%d", g.next()), "stop", 1, 1), nil
}

func (g *kbStreamGen) GenerateStream(context.Context, GenerateRequest) (<-chan Chunk, <-chan Result, error) {
	n := g.next()
	chunks := make(chan Chunk, 1)
	results := make(chan Result, 1)
	go func() {
		chunks <- Chunk{Content: fmt.Sprintf("stream-%d", n)}
		close(chunks)
		results <- NewTextResult(fmt.Sprintf("stream-%d", n), "stop", 1, 1)
		close(results)
	}()
	return chunks, results, nil
}

// L3 — under keep-best, only attempt 0 streams; OnStreamEnd(0) fires and the
// silent retry runs on the sync path, so the caller never sees a second draft.
func TestOnStreamEnd_FiresBeforeSilentRetry(t *testing.T) {
	ctx := context.Background()
	g := &kbStreamGen{}
	e, _ := reproEngine(t, "l3", map[string]Generator{"g": g}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	var calls int
	e.Hooks().RegisterPost("kb", func(_ context.Context, _ *StepRequest, res Result) (Result, error) {
		calls++
		if calls == 1 {
			return nil, ErrRetryScored(RetryAnnotation{Reason: "again"}, 5, res)
		}
		return res, nil
	})

	var chunks []string
	var streamEnds []int
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	step, err := e.RunStep(ctx, sess, StepRequest{
		AgentSlug:   "a",
		RetryMode:   RetryKeepBest,
		OnChunk:     func(c Chunk) { chunks = append(chunks, c.Content) },
		OnStreamEnd: func(attempt int) { streamEnds = append(streamEnds, attempt) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0] != "stream-1" {
		t.Fatalf("caller saw chunks %v, want only [stream-1]", chunks)
	}
	if len(streamEnds) != 1 || streamEnds[0] != 0 {
		t.Fatalf("OnStreamEnd calls %v, want [0]", streamEnds)
	}
	if got := ResultText(step.Result); got != "sync-2" {
		t.Fatalf("kept %q, want sync-2 (the silent retry's draft)", got)
	}
}

// L4 — a post-hook self-caps by counting its own accumulated annotations, then
// accepts; Attempt() reports the 0-based attempt at acceptance.
func TestHookSelfCap_UsesAttemptAndAnnotations(t *testing.T) {
	ctx := context.Background()
	g := &kbGen{}
	e, _ := reproEngine(t, "l4", map[string]Generator{"g": g}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	acceptedAttempt := -1
	e.Hooks().RegisterPost("cap", func(_ context.Context, req *StepRequest, res Result) (Result, error) {
		if len(req.RetryAnnotations()) < 2 {
			return nil, ErrRetryWith(RetryAnnotation{Reason: "cap"})
		}
		acceptedAttempt = req.Attempt()
		return res, nil
	})
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}
	if g.count() != 3 {
		t.Fatalf("generator called %d times, want 3 (two retries then accept)", g.count())
	}
	if acceptedAttempt != 2 {
		t.Fatalf("accepted at attempt %d, want 2", acceptedAttempt)
	}
}

// L4 — TurnRole is visible to hooks: "lead" for the lead, "follower:<slug>" for
// followers.
func TestTurnRole_VisibleToHooks(t *testing.T) {
	ctx := context.Background()
	g := &kbGen{}
	e, _ := reproEngine(t, "role", map[string]Generator{"g": g}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "lead", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Agents().Create(ctx, &Agent{Slug: "fol", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	roles := map[string]bool{}
	e.Hooks().RegisterPost("role", func(_ context.Context, req *StepRequest, res Result) (Result, error) {
		mu.Lock()
		roles[req.TurnRole()] = true
		mu.Unlock()
		return res, nil
	})
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunTurn(ctx, sess, TurnRequest{
		Flow:   Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}, Followers: []FlowAgent{{AgentSlug: "fol"}}},
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !roles["lead"] || !roles["follower:fol"] {
		t.Fatalf("roles seen = %v, want lead + follower:fol", roles)
	}
}

// L5 — a literal system-prompt override reaches the generator verbatim and is
// persisted on the step.
func TestPromptRefLiteral(t *testing.T) {
	ctx := context.Background()
	gen := &recordGen{}
	e, _ := reproEngine(t, "lit", map[string]Generator{"g": gen}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	step, err := e.RunStep(ctx, sess, StepRequest{
		AgentSlug:            "a",
		SystemPromptOverride: &PromptRef{Literal: "SYS-LITERAL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.prompts) != 1 || gen.prompts[0] != "SYS-LITERAL" {
		t.Fatalf("generator saw %v, want [SYS-LITERAL]", gen.prompts)
	}
	if step.Request.SystemPrompt != "SYS-LITERAL" {
		t.Fatalf("persisted system prompt = %q, want SYS-LITERAL", step.Request.SystemPrompt)
	}
}

// promptRecGen records the (system, user) prompt pair of every Generate call,
// keyed by system prompt, so a turn test can prove each agent got its own.
type promptRecGen struct {
	mu    sync.Mutex
	pairs map[string]string
}

func (g *promptRecGen) Modality() Modality { return ModalityText }
func (g *promptRecGen) Generate(_ context.Context, req GenerateRequest) (Result, error) {
	g.mu.Lock()
	if g.pairs == nil {
		g.pairs = map[string]string{}
	}
	g.pairs[req.SystemPrompt] = req.UserPrompt
	g.mu.Unlock()
	return NewTextResult("ok", "stop", 1, 1), nil
}

// Per-agent prompts — each agent in a turn receives its own literal system
// prompt and its own user_prompt input, not the turn-shared one.
func TestFlowAgent_PerAgentPrompt(t *testing.T) {
	ctx := context.Background()
	gen := &promptRecGen{}
	e, _ := reproEngine(t, "peragent", map[string]Generator{"g": gen}, PollerConfig{})
	ut := &Prompt{Slug: "ut", Version: 1, Kind: PromptKindUserTemplate, Body: `{{index .Inputs "user_prompt"}}`}
	if err := e.Prompts().Create(ctx, ut); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"a1", "a2", "a3"} {
		if err := e.Agents().Create(ctx, &Agent{Slug: slug, Version: 1, Modal: ModalityText, GeneratorSlug: "g", UserTemplateID: ut.ID}); err != nil {
			t.Fatal(err)
		}
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunTurn(ctx, sess, TurnRequest{
		Flow: Flow{
			Slug: "t",
			Lead: FlowAgent{AgentSlug: "a1", SystemPrompt: PromptRef{Literal: "sys-1"}, Inputs: map[string]any{"user_prompt": "usr-1"}},
			Followers: []FlowAgent{
				{AgentSlug: "a2", SystemPrompt: PromptRef{Literal: "sys-2"}, Inputs: map[string]any{"user_prompt": "usr-2"}},
				{AgentSlug: "a3", SystemPrompt: PromptRef{Literal: "sys-3"}, Inputs: map[string]any{"user_prompt": "usr-3"}},
			},
		},
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "go"}},
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"sys-1": "usr-1", "sys-2": "usr-2", "sys-3": "usr-3"}
	for sys, usr := range want {
		if gen.pairs[sys] != usr {
			t.Fatalf("agent with system %q got user %q, want %q (all: %v)", sys, gen.pairs[sys], usr, gen.pairs)
		}
	}
	if len(gen.pairs) != 3 {
		t.Fatalf("recorded %d distinct system prompts, want 3: %v", len(gen.pairs), gen.pairs)
	}
}

// L6 — a turn can mix retry modes: a keep-best lead survives exhaustion with its
// best draft while a discard follower fails on exhaustion.
func TestFlowAgentRetryModes(t *testing.T) {
	ctx := context.Background()
	lg := &kbGen{}
	fg := &kbGen{}
	e, _ := reproEngine(t, "l6", map[string]Generator{"lead-g": lg, "fol-g": fg}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "lead", Version: 1, Modal: ModalityText, GeneratorSlug: "lead-g"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Agents().Create(ctx, &Agent{Slug: "fol", Version: 1, Modal: ModalityText, GeneratorSlug: "fol-g"}); err != nil {
		t.Fatal(err)
	}
	e.Hooks().RegisterPost("h", func(_ context.Context, req *StepRequest, res Result) (Result, error) {
		if req.TurnRole() == "lead" {
			return nil, ErrRetryScored(RetryAnnotation{Reason: "x"}, 2, res) // always reject → keep best
		}
		return nil, ErrRetryWith(RetryAnnotation{Reason: "y"}) // follower: discard exhausts
	})
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	turn, err := e.RunTurn(ctx, sess, TurnRequest{
		Flow: Flow{
			Slug:      "t",
			Lead:      FlowAgent{AgentSlug: "lead", RetryMode: RetryKeepBest, MaxRetries: 1},
			Followers: []FlowAgent{{AgentSlug: "fol", RetryMode: RetryDiscard, MaxRetries: 1}},
		},
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
	})
	if err != nil {
		t.Fatalf("turn should succeed (lead keeps best): %v", err)
	}
	if turn.Lead == nil || ResultText(turn.Lead.Result) != "draft-1" {
		t.Fatalf("lead kept %v, want draft-1", turn.Lead)
	}
	if _, ok := turn.Followers["fol"]; ok {
		t.Fatal("discard follower exhausted retries; it should be absent from Followers")
	}
}
