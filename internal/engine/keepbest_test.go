package engine

// keepbest_test.go — proves L1 (per-request MaxRetries) and L2 (RetryKeepBest):
// the engine scores every attempt, persists the lowest-scoring draft, never
// fails on exhaustion, and keeps the best draft when a later generator call
// fails. RetryDiscard behavior is unchanged (covered by the existing tests).

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// kbGen returns a distinct result per call ("draft-1", "draft-2", …) and can be
// told to fail on the Nth (1-based) call.
type kbGen struct {
	mu     sync.Mutex
	calls  int
	failOn int
}

func (g *kbGen) Modality() Modality { return ModalityText }

func (g *kbGen) Generate(context.Context, GenerateRequest) (Result, error) {
	g.mu.Lock()
	g.calls++
	n := g.calls
	g.mu.Unlock()
	if g.failOn == n {
		return nil, fmt.Errorf("gen boom on call %d", n)
	}
	return NewTextResult(fmt.Sprintf("draft-%d", n), "stop", 1, 1), nil
}

func (g *kbGen) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// kbStep scripts one post-hook outcome. ok=true returns success; otherwise it
// returns ErrRetryScored with the given score (and the current draft unless
// noDraft).
type kbStep struct {
	ok      bool
	score   int
	noDraft bool
}

func kbHook(steps ...kbStep) PostHook {
	var i int
	return func(_ context.Context, _ *StepRequest, res Result) (Result, error) {
		s := steps[len(steps)-1]
		if i < len(steps) {
			s = steps[i]
		}
		i++
		if s.ok {
			return res, nil
		}
		draft := res
		if s.noDraft {
			draft = nil
		}
		return nil, ErrRetryScored(RetryAnnotation{Reason: "kb"}, s.score, draft)
	}
}

func runKB(t *testing.T, gen Generator, hook PostHook, mode RetryMode, maxRetries int) (*Step, error) {
	t.Helper()
	ctx := context.Background()
	e, _ := reproEngine(t, "kb", map[string]Generator{"g": gen}, PollerConfig{})
	if hook != nil {
		e.Hooks().RegisterPost("score", hook)
	}
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return e.RunStep(ctx, sess, StepRequest{AgentSlug: "a", RetryMode: mode, MaxRetries: maxRetries})
}

// L1 — a per-request MaxRetries caps the loop even when the engine default is 3.
func TestMaxRetries_RequestOverride(t *testing.T) {
	g := &kbGen{}
	_, err := runKB(t, g, kbHook(kbStep{score: 1}, kbStep{score: 1}, kbStep{score: 1}, kbStep{score: 1}), RetryDiscard, 1)
	if err == nil {
		t.Fatal("want a max-retries error under RetryDiscard")
	}
	if g.count() != 2 {
		t.Fatalf("generator called %d times, want 2 (attempt 0 + one retry)", g.count())
	}
}

// L2 — keep the first draft when it scores lowest and no later draft beats it.
func TestKeepBest_KeepFirst(t *testing.T) {
	g := &kbGen{}
	step, err := runKB(t, g, kbHook(kbStep{score: 2}, kbStep{score: 3}), RetryKeepBest, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResultText(step.Result); got != "draft-1" {
		t.Fatalf("kept %q, want draft-1 (lower score)", got)
	}
	if step.Diagnostics["kept_score"] != 2 {
		t.Fatalf("kept_score = %v, want 2", step.Diagnostics["kept_score"])
	}
}

// L2 — keep a later draft when it scores lower.
func TestKeepBest_KeepRetry(t *testing.T) {
	g := &kbGen{}
	step, err := runKB(t, g, kbHook(kbStep{score: 3}, kbStep{score: 1}), RetryKeepBest, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResultText(step.Result); got != "draft-2" {
		t.Fatalf("kept %q, want draft-2 (lower score)", got)
	}
	if step.Diagnostics["kept_score"] != 1 {
		t.Fatalf("kept_score = %v, want 1", step.Diagnostics["kept_score"])
	}
}

// L2 — a tie keeps the earlier draft (strict less-than).
func TestKeepBest_TieKeepsEarlier(t *testing.T) {
	g := &kbGen{}
	step, err := runKB(t, g, kbHook(kbStep{score: 2}, kbStep{score: 2}), RetryKeepBest, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResultText(step.Result); got != "draft-1" {
		t.Fatalf("kept %q, want draft-1 on a tie", got)
	}
}

// L2 — a non-positive score with a draft is accepted immediately (one call).
func TestKeepBest_ScoreZeroAccepts(t *testing.T) {
	g := &kbGen{}
	step, err := runKB(t, g, kbHook(kbStep{score: 0}), RetryKeepBest, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResultText(step.Result); got != "draft-1" {
		t.Fatalf("kept %q, want draft-1", got)
	}
	if g.count() != 1 {
		t.Fatalf("generator called %d times, want 1 (score-0 accepts, no retry)", g.count())
	}
	if step.Diagnostics["kept_score"] != 0 {
		t.Fatalf("kept_score = %v, want 0", step.Diagnostics["kept_score"])
	}
}

// L2 — exhaustion keeps the best draft under keep-best; the same script errors
// under discard.
func TestKeepBest_ExhaustionKeepsWhileDiscardErrors(t *testing.T) {
	g1 := &kbGen{}
	step, err := runKB(t, g1, kbHook(kbStep{score: 2}, kbStep{score: 3}), RetryKeepBest, 1)
	if err != nil {
		t.Fatalf("keep-best exhaustion should succeed, got: %v", err)
	}
	if ResultText(step.Result) != "draft-1" {
		t.Fatalf("kept %q, want draft-1", ResultText(step.Result))
	}

	g2 := &kbGen{}
	if _, err := runKB(t, g2, kbHook(kbStep{score: 2}, kbStep{score: 3}), RetryDiscard, 1); err == nil {
		t.Fatal("discard should error on exhausted retries")
	}
}

// L2 — a generator failure after attempt 0 keeps the best draft so far.
func TestKeepBest_LateGenFailureKeepsBest(t *testing.T) {
	g := &kbGen{failOn: 2} // attempt 0 succeeds, attempt 1's generator call fails
	step, err := runKB(t, g, kbHook(kbStep{score: 2}, kbStep{score: 2}, kbStep{score: 2}), RetryKeepBest, 3)
	if err != nil {
		t.Fatalf("keep-best should absorb a late generator failure, got: %v", err)
	}
	if got := ResultText(step.Result); got != "draft-1" {
		t.Fatalf("kept %q, want draft-1 (best before the failure)", got)
	}
}

// L2 — when no attempt ever yields a keepable draft, the step errors.
func TestKeepBest_NoUsableDraftErrors(t *testing.T) {
	g := &kbGen{}
	if _, err := runKB(t, g, kbHook(kbStep{score: 2, noDraft: true}, kbStep{score: 2, noDraft: true}), RetryKeepBest, 1); err == nil {
		t.Fatal("want an error when no attempt produces a keepable draft")
	}
}
