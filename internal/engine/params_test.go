package engine

// params_test.go — proves RunStep does not mutate the caller's (or a shared,
// cached) agent's Params.Extra. The free-form req.Params used to be folded into
// the agent's own Extra map in place, leaking one turn's params into the next and
// racing concurrent steps that share the agent.

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestParamsExtraNotMutated(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "params_alias", map[string]Generator{"g": okGen{}}, PollerConfig{})

	agent := &Agent{
		ID: uuid.New(), Slug: "pa", Version: 1, Modal: ModalityText, GeneratorSlug: "g",
		Params: GenerateParams{Extra: map[string]any{"base": "x"}},
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Reuse the SAME agent struct across two steps, each injecting different
	// params. The old code accumulated both into agent.Params.Extra.
	for i, inj := range []string{"one", "two"} {
		if _, err := e.RunStep(ctx, sess, StepRequest{Agent: agent, Params: map[string]any{"injected": inj}}); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}

	if len(agent.Params.Extra) != 1 || agent.Params.Extra["base"] != "x" {
		t.Errorf("RunStep mutated the agent's Params.Extra: got %v, want map[base:x]", agent.Params.Extra)
	}
}
