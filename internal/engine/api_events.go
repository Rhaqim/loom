package engine

// api_events.go: the session event stream. Every step, streamed chunk, turn
// completion and fork on a session is published to any number of subscribers,
// so every player watching a shared session sees the game master's narration
// arrive, not just the request that triggered it.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SessionEventType names a SessionEvent.
type SessionEventType string

const (
	// EventStepStarted fires when a step begins (before generation).
	EventStepStarted SessionEventType = "step.started"
	// EventChunk carries one streamed fragment of a step's output. Chunks are
	// live only: they are never replayed.
	EventChunk SessionEventType = "chunk"
	// EventStepDone fires once a step is persisted. Step is set.
	EventStepDone SessionEventType = "step.done"
	// EventStepFailed fires when a step fails before persisting. Err is set.
	EventStepFailed SessionEventType = "step.failed"
	// EventTurnDone fires when RunTurn finishes (lead and all followers).
	EventTurnDone SessionEventType = "turn.done"
	// EventForked fires on the parent when a branch is forked from it;
	// BranchID is the new session.
	EventForked SessionEventType = "forked"
)

// SessionEvent is one event on a session's stream. Treat it as read-only: the
// same value is delivered to every subscriber.
type SessionEvent struct {
	Type      SessionEventType
	SessionID uuid.UUID
	// StepIndex is the step's index for step.done, and the index of the last
	// persisted step a subscriber had seen for replayed events. It is the
	// resume cursor: reconnect with SubscribeOptions.FromStep = StepIndex+1.
	// -1 when the event is not about a persisted step.
	StepIndex int
	Step      *Step  // step.done
	Chunk     string // chunk
	Agent     string // agent slug, for step and chunk events
	TurnID    uuid.UUID
	TurnRole  string // "lead", "follower:<slug>" or "" outside a turn
	Err       string // step.failed
	BranchID  uuid.UUID
	// Replayed marks a step.done rebuilt from storage on Subscribe, rather
	// than delivered live.
	Replayed bool
	At       time.Time
}

// ErrSubscriberLagged is the reason a subscription was closed because its
// consumer fell too far behind. Reconnect with FromStep set past the last
// step.done received; nothing persisted is lost.
var ErrSubscriberLagged = errors.New("loom: event subscriber fell behind and was disconnected")

// Broadcaster fans session events out to subscribers. The default is
// in-process; a deployment with several engine instances supplies one backed
// by Redis, NATS or similar via Config.Broadcaster, so a player connected to
// one instance sees steps run on another.
type Broadcaster interface {
	// Publish delivers ev to the session's current subscribers. It must not
	// block on slow subscribers.
	Publish(ctx context.Context, ev SessionEvent)
	// Subscribe returns a live feed of the session's events from now on. The
	// channel is closed when ctx ends, when cancel is called, or when the
	// subscriber falls behind (lagged then returns true).
	Subscribe(ctx context.Context, sessionID uuid.UUID) (events <-chan SessionEvent, lagged func() bool, cancel func())
}

// SubscribeOptions configures Engine.Subscribe.
type SubscribeOptions struct {
	// FromStep replays every persisted step with Index >= FromStep as
	// step.done events (Replayed = true) before live events. 0 replays the
	// whole session; a negative value replays nothing.
	FromStep int
}

// Subscription is a live session event feed from Engine.Subscribe.
type Subscription struct {
	// Events delivers replayed then live events, in order. It is closed when
	// the subscription ends; call Err to learn why.
	Events <-chan SessionEvent
	err    func() error
	cancel func()
}

// Err reports why Events closed: nil after Close or a cancelled context,
// ErrSubscriberLagged when the consumer fell behind, or a replay error.
func (s *Subscription) Err() error { return s.err() }

// Close ends the subscription.
func (s *Subscription) Close() { s.cancel() }

// subscriberBuffer is how many undelivered events a subscriber may hold before
// it is disconnected as lagged.
const subscriberBuffer = 512

// memoryBroadcaster is the default in-process Broadcaster.
type memoryBroadcaster struct {
	mu   sync.Mutex
	subs map[uuid.UUID]map[*memorySub]struct{}
}

type memorySub struct {
	ch     chan SessionEvent
	lagged bool
	closed bool
}

func newMemoryBroadcaster() *memoryBroadcaster {
	return &memoryBroadcaster{subs: map[uuid.UUID]map[*memorySub]struct{}{}}
}

func (b *memoryBroadcaster) Publish(_ context.Context, ev SessionEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs[ev.SessionID] {
		select {
		case s.ch <- ev:
		default:
			// Full: disconnect rather than block the engine or drop silently.
			s.lagged = true
			b.closeLocked(ev.SessionID, s)
		}
	}
}

func (b *memoryBroadcaster) Subscribe(ctx context.Context, sessionID uuid.UUID) (<-chan SessionEvent, func() bool, func()) {
	s := &memorySub{ch: make(chan SessionEvent, subscriberBuffer)}
	b.mu.Lock()
	if b.subs[sessionID] == nil {
		b.subs[sessionID] = map[*memorySub]struct{}{}
	}
	b.subs[sessionID][s] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	done := make(chan struct{})
	cancel := func() {
		once.Do(func() {
			close(done)
			b.mu.Lock()
			b.closeLocked(sessionID, s)
			b.mu.Unlock()
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-done:
		}
	}()
	lagged := func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return s.lagged
	}
	return s.ch, lagged, cancel
}

func (b *memoryBroadcaster) closeLocked(sessionID uuid.UUID, s *memorySub) {
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
	delete(b.subs[sessionID], s)
	if len(b.subs[sessionID]) == 0 {
		delete(b.subs, sessionID)
	}
}
