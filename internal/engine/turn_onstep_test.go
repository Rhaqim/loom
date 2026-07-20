package engine

// turn_onstep_test.go — proves L7 (OnStep(role, step, err) fired outside the
// follower mutex, lead before followers, Turn.Errors on failure) and L8
// (session_conflict diagnostic on a CAS miss).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type errGen struct{}

func (errGen) Modality() Modality { return ModalityText }
func (errGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return nil, fmt.Errorf("gen down")
}

// L7 — the lead's OnStep fires before any follower's.
func TestOnStep_LeadFiresBeforeFollowers(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "onstep", map[string]Generator{"g": &kbGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	mustAgent(t, e, "f1", "g")

	var mu sync.Mutex
	var order []string
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunTurn(ctx, sess, TurnRequest{
		Flow:   Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}, Followers: []FlowAgent{{AgentSlug: "f1"}}},
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
		OnStep: func(role string, _ *Step, _ error) {
			mu.Lock()
			order = append(order, role)
			mu.Unlock()
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "lead" {
		t.Fatalf("OnStep order = %v, want lead first then the follower", order)
	}
}

// L7 — a follower whose generator errors surfaces as OnStep(role, nil, err) and
// a Turn.Errors entry, while its sibling completes normally.
func TestOnStep_FollowerErrorRecorded(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ferr", map[string]Generator{"ok-g": &kbGen{}, "bad-g": errGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "ok-g")
	mustAgent(t, e, "good", "ok-g")
	mustAgent(t, e, "bad", "bad-g")

	var mu sync.Mutex
	hadErr := map[string]bool{}
	nilStep := map[string]bool{}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	turn, err := e.RunTurn(ctx, sess, TurnRequest{
		Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"},
			Followers: []FlowAgent{{AgentSlug: "good"}, {AgentSlug: "bad"}}},
		Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
		OnStep: func(role string, step *Step, e error) {
			mu.Lock()
			hadErr[role] = e != nil
			nilStep[role] = step == nil
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := turn.Followers["good"]; !ok {
		t.Fatal("good follower missing from Followers")
	}
	if _, ok := turn.Followers["bad"]; ok {
		t.Fatal("bad follower should be absent from Followers")
	}
	if turn.Errors["bad"] == nil {
		t.Fatal("bad follower should have a Turn.Errors entry")
	}
	if !hadErr["follower:bad"] || !nilStep["follower:bad"] {
		t.Fatal("OnStep for bad should carry the error and a nil step")
	}
	if hadErr["follower:good"] {
		t.Fatal("OnStep for good should carry no error")
	}
}

// L7 — a blocked OnStep on one follower must not stall another's OnStep: the
// callbacks fire outside the shared mutex. (Also exercised under -race.)
func TestOnStep_FollowersNotSerialized(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "noser", map[string]Generator{"g": &kbGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	mustAgent(t, e, "f1", "g")
	mustAgent(t, e, "f2", "g")

	f1Blocked := make(chan struct{})
	f2Done := make(chan struct{})
	done := make(chan struct{})
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(done)
		_, _ = e.RunTurn(ctx, sess, TurnRequest{
			Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"},
				Followers: []FlowAgent{{AgentSlug: "f1"}, {AgentSlug: "f2"}}},
			Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hi"}},
			OnStep: func(role string, _ *Step, _ error) {
				switch role {
				case "follower:f1":
					<-f1Blocked // hold f1's callback open
				case "follower:f2":
					close(f2Done) // must be reachable while f1 is blocked
				}
			},
		})
	}()
	select {
	case <-f2Done:
		// f2's OnStep ran while f1's was still blocked — not serialized.
	case <-time.After(3 * time.Second):
		t.Fatal("f2 OnStep never ran while f1 was blocked — followers are serialized")
	}
	close(f1Blocked)
	<-done
}

// L8 — a CAS miss on the session Update flags the losing step AND is reported
// to the caller. The step itself is valid and durable, so RunStep returns it
// alongside ErrSessionNotPersisted (wrapping ErrSessionConflict) rather than
// reporting success on state the database does not have.
func TestSessionConflict_Diagnostic(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "conflict", map[string]Generator{"g": &kbGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale cross-instance writer by rolling our version back so the
	// next Update's compare-and-set misses.
	sess.Version--
	step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
	if !errors.Is(err, ErrSessionNotPersisted) {
		t.Fatalf("err = %v, want ErrSessionNotPersisted", err)
	}
	if !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("err = %v, want it to wrap ErrSessionConflict", err)
	}
	// The step ran and committed, so it must still come back usable.
	if step == nil {
		t.Fatal("step = nil, want the committed step alongside the error")
	}
	if step.Diagnostics["session_conflict"] != true {
		t.Fatalf("session_conflict diagnostic = %v, want true", step.Diagnostics["session_conflict"])
	}
	// The checkpoint is written inside the step transaction, so the authoritative
	// post-step state is recoverable even though the session row is stale.
	if _, found, serr := e.Sessions().StateAt(ctx, sess.ID, step.Index); serr != nil || !found {
		t.Fatalf("StateAt(%d) = found %v, err %v; want a recoverable checkpoint", step.Index, found, serr)
	}
}

func mustAgent(t *testing.T, e *Engine, slug, gen string) {
	t.Helper()
	if err := e.Agents().Create(context.Background(), &Agent{Slug: slug, Version: 1, Modal: ModalityText, GeneratorSlug: gen}); err != nil {
		t.Fatal(err)
	}
}
