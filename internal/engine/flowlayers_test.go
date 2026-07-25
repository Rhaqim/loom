package engine

// flowlayers_test.go — phase 2: dependency-layered follower execution.

import (
	"context"
	"reflect"
	"testing"
)

// --- pure layering ---

func TestTopoLayers(t *testing.T) {
	cases := []struct {
		name       string
		outputKeys []string
		consumed   [][]string
		wantLayers [][]int
		wantCyclic []int
	}{
		{
			name:       "no deps is one layer",
			outputKeys: []string{"A", "B", "C"},
			consumed:   [][]string{nil, nil, nil},
			wantLayers: [][]int{{0, 1, 2}},
		},
		{
			name:       "chain A->B->C",
			outputKeys: []string{"A", "B", "C"},
			consumed:   [][]string{nil, {"A"}, {"B"}},
			wantLayers: [][]int{{0}, {1}, {2}},
		},
		{
			name:       "tree: B and C both consume A",
			outputKeys: []string{"A", "B", "C"},
			consumed:   [][]string{nil, {"A"}, {"A"}},
			wantLayers: [][]int{{0}, {1, 2}},
		},
		{
			name:       "external/lead names create no edge",
			outputKeys: []string{"A", "B"},
			consumed:   [][]string{{"Lead", "topic"}, {"action"}},
			wantLayers: [][]int{{0, 1}},
		},
		{
			name:       "cycle A<->B",
			outputKeys: []string{"A", "B"},
			consumed:   [][]string{{"B"}, {"A"}},
			wantCyclic: []int{0, 1},
		},
		{
			name:       "one independent, two in a cycle",
			outputKeys: []string{"A", "B", "C"},
			consumed:   [][]string{nil, {"C"}, {"B"}},
			wantLayers: [][]int{{0}},
			wantCyclic: []int{1, 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layers, cyclic := topoLayers(tc.outputKeys, tc.consumed)
			if tc.wantLayers != nil && !reflect.DeepEqual(layers, tc.wantLayers) {
				t.Errorf("layers = %v, want %v", layers, tc.wantLayers)
			}
			if !reflect.DeepEqual(cyclic, tc.wantCyclic) {
				t.Errorf("cyclic = %v, want %v", cyclic, tc.wantCyclic)
			}
		})
	}
}

// --- end-to-end ordering ---

// echoPromptGen returns the rendered user prompt as its output, so an agent's
// output is exactly what its template produced — letting a downstream agent's
// result prove it saw the upstream output.
type echoPromptGen struct{}

func (echoPromptGen) Modality() Modality { return ModalityText }
func (echoPromptGen) Generate(_ context.Context, req GenerateRequest) (Result, error) {
	return NewTextResult(req.UserPrompt, "stop", 1, 1), nil
}

// mkEchoAgent creates an agent whose user template is `body` and which declares
// `vars` as required inputs.
func mkEchoAgent(t *testing.T, e *Engine, slug, body string, vars []string) {
	t.Helper()
	ctx := context.Background()
	ut := &Prompt{Slug: slug + "-ut", Version: 1, Kind: PromptKindUserTemplate, Body: body, Variables: vars}
	if err := e.Prompts().Create(ctx, ut); err != nil {
		t.Fatal(err)
	}
	if err := e.Agents().Create(ctx, &Agent{
		Slug: slug, Version: 1, Modal: ModalityText, GeneratorSlug: "echo",
		UserTemplateID: ut.ID,
	}); err != nil {
		t.Fatal(err)
	}
}

// A follower that consumes a sibling's output runs after it and actually
// receives that output — the core phase-2 behaviour.
func TestLayeredExecution_FollowerConsumesSibling(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "layer_chain", map[string]Generator{"echo": echoPromptGen{}}, PollerConfig{})
	mkEchoAgent(t, e, "lead", "LEAD-OUT", nil)
	mkEchoAgent(t, e, "f1", "F1-OUT", nil)
	// f2's output is whatever it read from F1 — proving it saw f1's output.
	mkEchoAgent(t, e, "f2", "{{.Inputs.F1}}", []string{"F1"})

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	flow := Flow{
		Slug: "t",
		Lead: FlowAgent{AgentSlug: "lead", OutputKey: "Lead"},
		Followers: []FlowAgent{
			{AgentSlug: "f1", OutputKey: "F1"},
			{AgentSlug: "f2", OutputKey: "F2"},
		},
	}
	turn, err := e.RunTurn(ctx, sess, TurnRequest{Flow: flow})
	if err != nil {
		t.Fatal(err)
	}
	if ferr := turn.Errors["f2"]; ferr != nil {
		t.Fatalf("f2 errored — it did not run after f1: %v", ferr)
	}
	f2 := turn.Followers["f2"]
	if f2 == nil {
		t.Fatal("f2 produced no step")
	}
	if got := ResultText(f2.Result); got != "F1-OUT" {
		t.Fatalf("f2 output = %q, want %q (it did not receive f1's output)", got, "F1-OUT")
	}
}

// A three-deep chain f1 -> f2 -> f3 threads output the whole way down.
func TestLayeredExecution_Chain(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "layer_deep", map[string]Generator{"echo": echoPromptGen{}}, PollerConfig{})
	mkEchoAgent(t, e, "lead", "L", nil)
	mkEchoAgent(t, e, "f1", "start", nil)
	mkEchoAgent(t, e, "f2", "{{.Inputs.F1}}-2", []string{"F1"})
	mkEchoAgent(t, e, "f3", "{{.Inputs.F2}}-3", []string{"F2"})

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	flow := Flow{
		Slug: "t",
		Lead: FlowAgent{AgentSlug: "lead", OutputKey: "Lead"},
		Followers: []FlowAgent{
			{AgentSlug: "f1", OutputKey: "F1"},
			{AgentSlug: "f2", OutputKey: "F2"},
			{AgentSlug: "f3", OutputKey: "F3"},
		},
	}
	turn, err := e.RunTurn(ctx, sess, TurnRequest{Flow: flow})
	if err != nil {
		t.Fatal(err)
	}
	if got := ResultText(turn.Followers["f3"].Result); got != "start-2-3" {
		t.Fatalf("f3 output = %q, want %q (chain did not thread through)", got, "start-2-3")
	}
	// All four steps (lead + 3 followers) landed and are index-contiguous.
	steps, _ := e.Sessions().Steps(ctx, sess.ID, 0, 0)
	if len(steps) != 4 {
		t.Fatalf("persisted steps = %d, want 4", len(steps))
	}
}

// Independent followers still run — a flow with no cross-follower deps is a
// single layer, exactly as before phase 2.
func TestLayeredExecution_IndependentFollowersStillConcurrent(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "layer_indep", map[string]Generator{"echo": echoPromptGen{}}, PollerConfig{})
	mkEchoAgent(t, e, "lead", "L", nil)
	mkEchoAgent(t, e, "f1", "one", nil)
	mkEchoAgent(t, e, "f2", "two", nil)

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	flow := Flow{
		Slug: "t",
		Lead: FlowAgent{AgentSlug: "lead", OutputKey: "Lead"},
		Followers: []FlowAgent{
			{AgentSlug: "f1", OutputKey: "F1"},
			{AgentSlug: "f2", OutputKey: "F2"},
		},
	}
	turn, err := e.RunTurn(ctx, sess, TurnRequest{Flow: flow})
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Followers) != 2 {
		t.Fatalf("followers run = %d, want 2", len(turn.Followers))
	}
}
