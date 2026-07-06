package loom

// retry_annotation_test.go — proves a post-hook's retry hints (Reason + Forbidden)
// actually reach the model. Before the fix, the engine set GenerateRequest.
// Annotations but every generator ignored that field, so retries re-ran the
// identical prompt; now the engine folds the hints into the system prompt.

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// recordGen records the SystemPrompt of each Generate call, so a test can assert
// what the model saw on the first attempt versus a retry.
type recordGen struct {
	mu      sync.Mutex
	prompts []string
}

func (g *recordGen) Modality() Modality { return ModalityText }

func (g *recordGen) Generate(_ context.Context, req GenerateRequest) (Result, error) {
	g.mu.Lock()
	g.prompts = append(g.prompts, req.SystemPrompt)
	g.mu.Unlock()
	return NewTextResult("ok", "stop", 1, 1), nil
}

// TestRetryAnnotationReachesPrompt is the discriminating test: it fails on the
// old engine (which left the forbidden phrase only on the ignored Annotations
// field) and passes once the hints are folded into the retry system prompt.
func TestRetryAnnotationReachesPrompt(t *testing.T) {
	ctx := context.Background()
	gen := &recordGen{}
	e, _ := reproEngine(t, "retryann", map[string]Generator{"g": gen}, PollerConfig{})

	const baseSys = "You are an assistant."
	sp := &Prompt{Slug: "sys", Version: 1, Kind: PromptKindSystem, Body: baseSys}
	if err := e.Prompts().Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g", SystemPromptID: sp.ID}); err != nil {
		t.Fatal(err)
	}

	// Reject exactly the first attempt, forbidding a phrase and giving a reason.
	var rejected bool
	e.Hooks().RegisterPost("ban", func(_ context.Context, req *StepRequest, res Result) (Result, error) {
		if !rejected {
			rejected = true
			return nil, ErrRetryWith(RetryAnnotation{Reason: "too cliché", Forbidden: []string{"down your spine"}})
		}
		return res, nil
	})

	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatalf("run step: %v", err)
	}

	if len(gen.prompts) != 2 {
		t.Fatalf("generator called %d times, want 2 (attempt + one retry)", len(gen.prompts))
	}
	first, retry := gen.prompts[0], gen.prompts[1]

	// First attempt: the clean base prompt, no directive.
	if first != baseSys {
		t.Errorf("first attempt system prompt = %q, want the clean base %q", first, baseSys)
	}
	// Retry: base preserved, plus the reason and the forbidden phrase.
	if !strings.HasPrefix(retry, baseSys) {
		t.Errorf("retry prompt dropped the base system prompt: %q", retry)
	}
	if !strings.Contains(retry, "down your spine") {
		t.Errorf("retry prompt missing the forbidden phrase: %q", retry)
	}
	if !strings.Contains(retry, "too cliché") {
		t.Errorf("retry prompt missing the rejection reason: %q", retry)
	}
}

func TestAnnotationDirective(t *testing.T) {
	if got := annotationDirective(nil); got != "" {
		t.Errorf("nil annotations = %q, want empty", got)
	}
	// De-dupes reasons and forbidden phrases accumulated across attempts.
	d := annotationDirective([]RetryAnnotation{
		{Reason: "cliché", Forbidden: []string{"spine", "thick air"}},
		{Reason: "cliché", Forbidden: []string{"spine", "heart raced"}},
	})
	for _, want := range []string{"cliché", "spine", "thick air", "heart raced"} {
		if !strings.Contains(d, want) {
			t.Errorf("directive %q missing %q", d, want)
		}
	}
	if n := strings.Count(d, "spine"); n != 1 {
		t.Errorf("directive did not dedup 'spine' (%d occurrences): %q", n, d)
	}
	if n := strings.Count(d, "cliché"); n != 1 {
		t.Errorf("directive did not dedup the reason (%d occurrences): %q", n, d)
	}
}

func TestWithRetryDirective(t *testing.T) {
	anns := []RetryAnnotation{{Forbidden: []string{"x"}}}
	if got := withRetryDirective("BASE", nil); got != "BASE" {
		t.Errorf("no annotations = %q, want BASE unchanged", got)
	}
	if got := withRetryDirective("", anns); got == "" || strings.HasPrefix(got, "\n") {
		t.Errorf("empty base with annotations = %q, want the directive without a leading newline", got)
	}
	if got := withRetryDirective("BASE", anns); !strings.HasPrefix(got, "BASE\n\n") {
		t.Errorf("base with annotations = %q, want %q", got, "BASE\\n\\n<directive>")
	}
}
