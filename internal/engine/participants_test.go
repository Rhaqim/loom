package engine

// participants_test.go: multi-participant sessions (client ask §1).

import (
	"context"
	"errors"
	"testing"
	"time"
)

func partySetup(t *testing.T) (*Engine, *Session) {
	t.Helper()
	ctx := context.Background()
	e, _ := reproEngine(t, "party", map[string]Generator{"g": okGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "gm", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "game:xyz", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return e, sess
}

func act(id string) *Action {
	return &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "go"}, ParticipantID: id}
}

func TestParticipantsMembershipAndActions(t *testing.T) {
	ctx := context.Background()
	e, sess := partySetup(t)
	if err := e.AddParticipant(ctx, sess, Participant{ID: "host", Role: "host"}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddParticipant(ctx, sess, Participant{ID: "host"}); !errors.Is(err, ErrParticipantExists) {
		t.Fatalf("duplicate add: err = %v", err)
	}
	step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "gm", Action: act("host")})
	if err != nil {
		t.Fatal(err)
	}
	if step.ParticipantID != "host" {
		t.Fatalf("step.ParticipantID = %q", step.ParticipantID)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "gm", Action: act("stranger")}); !errors.Is(err, ErrNotParticipant) {
		t.Fatalf("stranger's action: err = %v, want ErrNotParticipant", err)
	}

	got, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.State.HasParticipant("host") || got.State.Participants[0].Role != "host" || got.State.Participants[0].JoinedAt.IsZero() {
		t.Fatalf("persisted participants = %+v", got.State.Participants)
	}
	if got.History[0].ParticipantID != "host" {
		t.Fatalf("reloaded step participant = %q", got.History[0].ParticipantID)
	}

	if err := e.RemoveParticipant(ctx, sess, "host"); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveParticipant(ctx, sess, "host"); !errors.Is(err, ErrNotParticipant) {
		t.Fatalf("second remove: err = %v", err)
	}
}

// A fork from before a player joined drops that player; a fork after keeps them.
func TestForkRestoresTheParty(t *testing.T) {
	ctx := context.Background()
	e, sess := partySetup(t)
	_ = e.AddParticipant(ctx, sess, Participant{ID: "host", Role: "host"})
	s0, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "gm", Action: act("host")})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.AddParticipant(ctx, sess, Participant{ID: "wren", Role: "player"})
	s1, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "gm", Action: act("wren")})
	if err != nil {
		t.Fatal(err)
	}
	before, err := e.Sessions().Fork(ctx, sess.ID, s0.Index)
	if err != nil {
		t.Fatal(err)
	}
	if before.State.HasParticipant("wren") || !before.State.HasParticipant("host") {
		t.Fatalf("fork before the join = %+v", before.State.Participants)
	}
	after, err := e.Sessions().Fork(ctx, sess.ID, s1.Index)
	if err != nil {
		t.Fatal(err)
	}
	if !after.State.HasParticipant("wren") {
		t.Fatalf("fork after the join lost wren: %+v", after.State.Participants)
	}
}

// A player joining while a turn runs is neither lost by the turn's state merge
// nor reported as a session write conflict.
func TestJoinDuringTurnIsKept(t *testing.T) {
	ctx := context.Background()
	e, sess := partySetup(t)
	if err := e.Agents().Create(ctx, &Agent{Slug: "slow", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	joined := make(chan error, 1)
	e.Hooks().RegisterPost("join-mid-turn", func(_ context.Context, req *StepRequest, res Result) (Result, error) {
		if req.TurnRole() == "follower:slow" {
			go func() { joined <- e.AddParticipant(ctx, sess, Participant{ID: "late", Role: "player"}) }()
			time.Sleep(30 * time.Millisecond)
		}
		return res, nil
	})
	turn, err := e.RunTurn(ctx, sess, TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "gm"}, Followers: []FlowAgent{{AgentSlug: "slow"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Errors) > 0 {
		t.Fatalf("turn errors: %v", turn.Errors)
	}
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	got, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.State.HasParticipant("late") {
		t.Fatalf("mid-turn join lost: %+v", got.State.Participants)
	}
}
