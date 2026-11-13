package engine

// merge_state_test.go: regression for the client-reported P0 "parallel
// followers overwrite each other's state". A follower that finished later
// replayed its stale Snapshot and every Vars key over a sibling's writes,
// because the merge compared it with the session's current state rather than
// with the state the follower started from.

import (
	"context"
	"slices"
	"testing"
	"time"
)

// The client's reproduction: A writes Snapshot and Vars["a"]; B writes only
// Vars["b"] and finishes 50 ms later. Both of A's writes must survive.
func TestParallelFollowersKeepEachOthersWrites(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "merge", map[string]Generator{"g": okGen{}}, PollerConfig{})
	for _, slug := range []string{"lead", "a", "b"} {
		if err := e.Agents().Create(ctx, &Agent{Slug: slug, Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
			t.Fatal(err)
		}
	}
	e.Hooks().RegisterPost("writes", func(_ context.Context, req *StepRequest, res Result) (Result, error) {
		st := &req.Session.State
		switch req.TurnRole() {
		case "follower:a":
			st.Snapshot = []byte(`{"hp":9}`)
			st.Vars["a"] = "set by a"
		case "follower:b":
			time.Sleep(50 * time.Millisecond)
			st.Vars["b"] = "set by b"
		}
		return res, nil
	})
	sess := &Session{PlatformID: "p", State: State{
		Modality: ModalityText,
		Snapshot: []byte(`{"hp":10}`),
		Vars:     map[string]any{"a": "before", "summary": "old"},
	}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunTurn(ctx, sess, TurnRequest{Flow: Flow{
		Slug: "t", Lead: FlowAgent{AgentSlug: "lead"},
		Followers: []FlowAgent{{AgentSlug: "a"}, {AgentSlug: "b"}},
	}}); err != nil {
		t.Fatal(err)
	}

	// In memory and as persisted.
	got, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]State{"in-memory": sess.State, "persisted": got.State} {
		if string(st.Snapshot) != `{"hp":9}` {
			t.Errorf("%s Snapshot = %s, want A's write {\"hp\":9}", name, st.Snapshot)
		}
		if st.Vars["a"] != "set by a" || st.Vars["b"] != "set by b" || st.Vars["summary"] != "old" {
			t.Errorf("%s Vars = %v, want a and b from their followers and summary untouched", name, st.Vars)
		}
	}
}

func TestMergeStateRules(t *testing.T) {
	start := State{
		Vars:     map[string]any{"keep": 1, "mine": 1, "gone": 1, "nested": map[string]any{"n": 1}},
		Snapshot: []byte("s0"),
	}
	base := baseOf(start)

	t.Run("applies only the step's own changes", func(t *testing.T) {
		// A sibling already changed "keep" and Snapshot.
		dst := State{Vars: map[string]any{"keep": 2, "mine": 1, "gone": 1, "nested": map[string]any{"n": 1}}, Snapshot: []byte("s-sibling")}
		step := State{Vars: map[string]any{"keep": 1, "mine": 5, "nested": map[string]any{"n": 1}}, Snapshot: []byte("s0")}
		if c := mergeState(&dst, base, step); len(c) != 0 {
			t.Fatalf("unexpected conflicts %v", c)
		}
		if dst.Vars["keep"] != 2 || dst.Vars["mine"] != 5 || string(dst.Snapshot) != "s-sibling" {
			t.Fatalf("merged = %v / %s", dst.Vars, dst.Snapshot)
		}
		if _, ok := dst.Vars["gone"]; ok {
			t.Fatal("a key the step deleted was kept")
		}
	})

	t.Run("in-place nested mutation is seen", func(t *testing.T) {
		// forStep clones Vars shallowly, so the step's nested map is the same
		// map the start state held; mutating it must still count as a change.
		cp := (&Session{State: start}).forStep().State
		b := baseOf(cp)
		cp.Vars["nested"].(map[string]any)["n"] = 2
		dst := State{Vars: map[string]any{}}
		mergeState(&dst, b, cp)
		if dst.Vars["nested"].(map[string]any)["n"] != 2 {
			t.Fatal("in-place nested write was dropped")
		}
	})

	t.Run("same key changed by both is a reported conflict, last writer wins", func(t *testing.T) {
		dst := State{Vars: map[string]any{"mine": 7}, Snapshot: []byte("s-sibling")}
		step := State{Vars: map[string]any{"mine": 5}, Snapshot: []byte("s-step")}
		c := mergeState(&dst, base, step)
		slices.Sort(c)
		if !slices.Equal(c, []string{"Snapshot", "Vars.mine"}) {
			t.Fatalf("conflicts = %v, want [Snapshot Vars.mine]", c)
		}
		if dst.Vars["mine"] != 5 || string(dst.Snapshot) != "s-step" {
			t.Fatalf("last writer did not win: %v / %s", dst.Vars, dst.Snapshot)
		}
	})
}
