package engine

import (
	"context"
	"maps"
	"time"

	"github.com/google/uuid"
)

// Session is a running conversation. It holds state, action log, result history,
// and any custom data the platform attaches.
type Session struct {
	ID              uuid.UUID
	PlatformID      string     // opaque — set by the platform (user ID, tenant ID, etc.)
	ParentSessionID *uuid.UUID // non-nil for branch sessions
	BranchPoint     *int       // step index the branch forks from
	// Version is the optimistic-concurrency revision. Update writes are guarded
	// by it (compare-and-set) so a stale writer cannot silently clobber a newer
	// one; it is bumped on each successful Update.
	Version   int
	State     State
	History   []Step
	Tags      []string
	Pinned    bool
	DeletedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	Metadata  map[string]any // platform-controlled custom data
}

// State is the live state of a session between steps.
type State struct {
	Modality         Modality       // current dominant modality (the latest result kind)
	Snapshot         []byte         // serialized application state (opaque to engine)
	Vars             map[string]any // engine-tracked typed variables
	AvailableActions []ActionTemplate
}

// forStep returns a shallow copy of the session with a cloned State.Vars map so
// a step's pre-hooks and template render mutate and read private state instead of
// racing concurrent sibling steps in the same turn. Hook-written Vars are merged
// back onto the shared session via mergeVars under the engine lock.
func (s *Session) forStep() *Session {
	cp := *s
	cp.State.Vars = maps.Clone(s.State.Vars)
	return &cp
}

// mergeVars copies a step's (possibly hook-mutated) Vars onto dst, lazily
// allocating dst.Vars. A no-op when vars is empty.
func mergeVars(dst *State, vars map[string]any) {
	if len(vars) == 0 {
		return
	}
	if dst.Vars == nil {
		dst.Vars = make(map[string]any, len(vars))
	}
	maps.Copy(dst.Vars, vars)
}

// BranchNode represents a node in the session branch tree.
type BranchNode struct {
	Session  *Session
	Children []*BranchNode
}

// SessionRegistry manages session persistence and branching.
type SessionRegistry interface {
	// Create persists a new session.
	Create(ctx context.Context, s *Session) error
	// Get retrieves a session by ID. A missing session returns *NotFoundError
	// (which unwraps to ErrNotFound).
	Get(ctx context.Context, id uuid.UUID) (*Session, error)
	// Update persists state changes to an existing session.
	Update(ctx context.Context, s *Session) error
	// Fork creates a branch session from parentID at the given step index.
	// The branch session sees parent steps 0..stepIndex; subsequent steps diverge.
	Fork(ctx context.Context, parentID uuid.UUID, stepIndex int) (*Session, error)
	// BranchTree returns the full descendant tree rooted at sessionID.
	BranchTree(ctx context.Context, rootID uuid.UUID) (*BranchNode, error)
	// Pin marks a session as exempt from garbage collection.
	Pin(ctx context.Context, id uuid.UUID) error
	// Unpin removes the GC exemption.
	Unpin(ctx context.Context, id uuid.UUID) error
	// Discard soft-deletes a session immediately (bypasses T1/T2 GC tiers).
	Discard(ctx context.Context, id uuid.UUID) error
	// Purge performs an immediate hard delete (irreversible).
	Purge(ctx context.Context, id uuid.UUID) error
	// List returns sessions for a platform ID.
	List(ctx context.Context, platformID string, limit, offset int) ([]*Session, error)
}
