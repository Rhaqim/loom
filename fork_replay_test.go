package loom

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestForkRewindsToCheckpoint verifies that Fork reconstructs session state as
// of the fork point from the per-step checkpoints, rather than inheriting the
// parent's latest state.
func TestForkRewindsToCheckpoint(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "fork", map[string]Generator{"g": okGen{}}, PollerConfig{})

	// Stamp a per-step counter into Vars so each step's checkpoint differs.
	var n int
	e.Hooks().RegisterPre("stamp", func(_ context.Context, req *StepRequest) error {
		n++
		if req.Session.State.Vars == nil {
			req.Session.State.Vars = map[string]any{}
		}
		req.Session.State.Vars["step"] = n
		return nil
	})

	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Three steps produce checkpoints: index 0 -> 1, index 1 -> 2, index 2 -> 3.
	for i := 0; i < 3; i++ {
		if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if got := sess.State.Vars["step"]; got != 3 {
		t.Fatalf("parent latest step var = %v, want 3", got)
	}

	// Each fork point must rewind to the checkpoint at or before that step.
	// (Vars round-trip through JSON, so values come back as float64.)
	cases := map[int]float64{0: 1, 1: 2, 5: 3} // 5 clamps to the latest checkpoint
	for at, want := range cases {
		branch, err := e.Sessions().Fork(ctx, sess.ID, at)
		if err != nil {
			t.Fatalf("fork at %d: %v", at, err)
		}
		got, ok := branch.State.Vars["step"]
		if !ok {
			t.Fatalf("fork at %d: branch did not inherit checkpoint vars", at)
		}
		if f, _ := got.(float64); f != want {
			t.Fatalf("fork at %d: step var = %v, want %v", at, got, want)
		}
		// The full State is reconstructed, not just Vars — Modality must be carried.
		if branch.State.Modality != ModalityText {
			t.Fatalf("fork at %d: modality = %q, want text (full state not reconstructed)", at, branch.State.Modality)
		}
	}
}

// TestSnapshotUniquePerStep proves the state_snapshots UNIQUE(session_id,
// step_index) invariant: a second checkpoint at the same step is rejected.
func TestSnapshotUniquePerStep(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "snapuniq", map[string]Generator{"g": okGen{}}, PollerConfig{})

	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// One step writes the checkpoint at step_index 0.
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}

	// A duplicate checkpoint at (session, step_index=0) must violate the UNIQUE.
	_, err := db.ExecContext(ctx,
		`INSERT INTO loom_state_snapshots (id, session_id, step_index, snapshot, vars, created_at)
		 VALUES ($1,$2,0,'{}','{}',CURRENT_TIMESTAMP)`, uuid.NewString(), sess.ID)
	if err == nil {
		t.Fatal("expected a UNIQUE(session_id, step_index) violation on a duplicate snapshot")
	}
}

// TestForkWithoutCheckpointFallsBack verifies Fork degrades gracefully to the
// parent's current state when no checkpoint exists at the fork point.
func TestForkWithoutCheckpointFallsBack(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "forkfb", map[string]Generator{"g": okGen{}}, PollerConfig{})

	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText, Vars: map[string]any{"seed": "v"}}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// No steps ran, so there are no checkpoints; forking must still succeed.
	branch, err := e.Sessions().Fork(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if branch.State.Vars["seed"] != "v" {
		t.Fatalf("fallback fork lost parent state: %+v", branch.State.Vars)
	}
}
