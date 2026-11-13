package engine

// events_test.go: the session event stream (client ask §2).

import (
	"context"
	"errors"
	"testing"
	"time"
)

// chunkGen streams its text in two chunks.
type chunkGen struct{}

func (chunkGen) Modality() Modality { return ModalityText }
func (chunkGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return NewTextResult("The gate opens.", "stop", 1, 1), nil
}
func (chunkGen) GenerateStream(context.Context, GenerateRequest) (<-chan Chunk, <-chan Result, error) {
	chunks, results := make(chan Chunk, 2), make(chan Result, 1)
	chunks <- Chunk{Content: "The gate "}
	chunks <- Chunk{Content: "opens."}
	close(chunks)
	results <- NewTextResult("The gate opens.", "stop", 1, 1)
	close(results)
	return chunks, results, nil
}

type failGen struct{}

func (failGen) Modality() Modality { return ModalityText }
func (failGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return nil, errors.New("provider down")
}

func eventsSetup(t *testing.T) (*Engine, *Session) {
	t.Helper()
	ctx := context.Background()
	e, _ := reproEngine(t, "events", map[string]Generator{"g": chunkGen{}, "bad": failGen{}}, PollerConfig{})
	for slug, gen := range map[string]string{"gm": "g", "ref": "g", "broken": "bad"} {
		if err := e.Agents().Create(ctx, &Agent{Slug: slug, Version: 1, Modal: ModalityText, GeneratorSlug: gen}); err != nil {
			t.Fatal(err)
		}
	}
	sess := &Session{PlatformID: "game:ev", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return e, sess
}

// next reads one event or fails after a timeout.
func next(t *testing.T, sub *Subscription) SessionEvent {
	t.Helper()
	select {
	case ev, ok := <-sub.Events:
		if !ok {
			t.Fatalf("subscription closed: %v", sub.Err())
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return SessionEvent{}
}

func TestEveryWatcherSeesTheStepLive(t *testing.T) {
	ctx := context.Background()
	e, sess := eventsSetup(t)
	a, err := e.Subscribe(ctx, sess.ID, SubscribeOptions{FromStep: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := e.Subscribe(ctx, sess.ID, SubscribeOptions{FromStep: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// Only the caller passes OnChunk; both watchers still get the chunks.
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "gm", OnChunk: func(Chunk) {}}); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*Subscription{a, b} {
		want := []SessionEventType{EventStepStarted, EventChunk, EventChunk, EventStepDone}
		var text string
		for i, typ := range want {
			ev := next(t, sub)
			if ev.Type != typ {
				t.Fatalf("event %d = %s, want %s", i, ev.Type, typ)
			}
			text += ev.Chunk
			if typ == EventStepDone && (ev.Step == nil || ev.StepIndex != 0 || ev.Agent != "gm") {
				t.Fatalf("step.done = %+v", ev)
			}
		}
		if text != "The gate opens." {
			t.Fatalf("chunks = %q", text)
		}
	}

	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "broken"}); err == nil {
		t.Fatal("broken agent succeeded")
	}
	next(t, a) // step.started
	if ev := next(t, a); ev.Type != EventStepFailed || ev.Err == "" || ev.Agent != "broken" {
		t.Fatalf("failure event = %+v", ev)
	}

	if _, err := e.RunTurn(ctx, sess, TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "gm"}, Followers: []FlowAgent{{AgentSlug: "ref"}}}}); err != nil {
		t.Fatal(err)
	}
	var sawTurnDone bool
	for i := 0; i < 5 && !sawTurnDone; i++ {
		ev := next(t, a)
		if ev.Type == EventStepDone && ev.TurnRole == "" {
			t.Fatalf("turn step without its role: %+v", ev)
		}
		sawTurnDone = ev.Type == EventTurnDone
	}
	if !sawTurnDone {
		t.Fatal("no turn.done")
	}
}

func TestResumeReplaysOnlyWhatWasMissed(t *testing.T) {
	ctx := context.Background()
	e, sess := eventsSetup(t)
	for range 3 {
		if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "gm"}); err != nil {
			t.Fatal(err)
		}
	}
	// The client saw steps 0 and 1 before disconnecting.
	sub, err := e.Subscribe(ctx, sess.ID, SubscribeOptions{FromStep: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if ev := next(t, sub); ev.Type != EventStepDone || !ev.Replayed || ev.StepIndex != 2 || ev.Step == nil {
		t.Fatalf("replay = %+v, want replayed step.done 2", ev)
	}
	// A late live copy of an already-replayed step is not delivered twice.
	e.emit(ctx, SessionEvent{Type: EventStepDone, SessionID: sess.ID, StepIndex: 2})
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "gm"}); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, sub); ev.Type != EventStepStarted {
		t.Fatalf("after replay got %+v, want the next live step.started (duplicate not dropped?)", ev)
	}
	if ev := next(t, sub); ev.Type != EventStepDone || ev.Replayed || ev.StepIndex != 3 {
		t.Fatalf("live = %+v, want step.done 3", ev)
	}
}

func TestLaggingSubscriberIsDisconnected(t *testing.T) {
	ctx := context.Background()
	e, sess := eventsSetup(t)
	sub, err := e.Subscribe(ctx, sess.ID, SubscribeOptions{FromStep: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for range subscriberBuffer + 200 {
		e.emit(ctx, SessionEvent{Type: EventChunk, SessionID: sess.ID, StepIndex: -1, Chunk: "x"})
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-sub.Events:
			if !ok {
				if !errors.Is(sub.Err(), ErrSubscriberLagged) {
					t.Fatalf("closed with %v, want ErrSubscriberLagged", sub.Err())
				}
				return
			}
		case <-deadline:
			t.Fatal("lagging subscriber was never disconnected")
		}
	}
}

func TestForkIsAnnounced(t *testing.T) {
	ctx := context.Background()
	e, sess := eventsSetup(t)
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "gm"}); err != nil {
		t.Fatal(err)
	}
	sub, err := e.Subscribe(ctx, sess.ID, SubscribeOptions{FromStep: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	branch, err := e.Sessions().Fork(ctx, sess.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ev := next(t, sub); ev.Type != EventForked || ev.BranchID != branch.ID {
		t.Fatalf("fork event = %+v", ev)
	}
}
