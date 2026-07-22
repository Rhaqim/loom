package engine

import (
	"bytes"
	"context"
	"maps"
	"reflect"
	"slices"
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

// forStep returns a copy of the session whose State a step's pre-hooks and
// template render can mutate privately, instead of racing concurrent sibling
// steps in the same turn. Hook-written State is merged back onto the shared
// session via mergeState under the engine lock.
//
// Every reference-typed field of State is deep-copied, not just Vars. A shallow
// copy would leave Snapshot and AvailableActions sharing their backing arrays,
// so an in-place write from a hook (snap[0] = x) would leak to siblings and
// survive, while a reassignment (State.Snapshot = ...) would not — the same
// operation persisting or not depending on how it was spelled. Deep-copying
// makes the rule uniform: nothing propagates except through mergeState.
func (s *Session) forStep() *Session {
	cp := *s
	cp.State.Vars = maps.Clone(s.State.Vars)
	cp.State.Snapshot = bytes.Clone(s.State.Snapshot)
	cp.State.AvailableActions = slices.Clone(s.State.AvailableActions)
	return &cp
}

// mergeState reconciles a step's (possibly hook-mutated) State back onto dst.
//
// Vars are merged key-by-key so concurrent followers each contribute their own
// keys. Snapshot and AvailableActions are opaque whole values the engine cannot
// merge, so they are replaced when the step changed them, and left alone
// otherwise — a hook that never touches Snapshot must not clear it. Concurrent
// followers that both write Snapshot resolve last-writer-wins.
func mergeState(dst *State, src State) {
	mergeVars(dst, src.Vars)
	if !bytes.Equal(dst.Snapshot, src.Snapshot) {
		dst.Snapshot = src.Snapshot
	}
	// reflect.DeepEqual, not slices.Equal: ActionTemplate.Payload and
	// .Constraints are `any` and may hold maps or slices, which would make ==
	// panic at runtime.
	if !reflect.DeepEqual(dst.AvailableActions, src.AvailableActions) {
		dst.AvailableActions = src.AvailableActions
	}
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
	Session *Session
	// StepCount is how many steps this session has recorded.
	//
	// It is counted by the tree query itself, because Session.History is NOT
	// loaded on a tree node — BranchTree deliberately reads session rows only,
	// so len(node.Session.History) is always 0 here and is not a step count. A
	// rewind or timeline UI needs the number per node, and fetching it with a
	// Steps() call per node would be N queries over a tree loom already walked.
	StepCount int
	Children  []*BranchNode
}

// SessionRegistry manages session persistence and branching.
type SessionRegistry interface {
	// Create persists a new session.
	Create(ctx context.Context, s *Session) error
	// Get retrieves a session by ID, with its full step history loaded. A
	// missing OR soft-deleted session returns *NotFoundError (which unwraps to
	// ErrNotFound) — consistent with List, BranchTree and Update. Use
	// GetIncludingDeleted to read a discarded session (e.g. to restore it).
	//
	// Get loads every step, and each step carries its fully rendered request and
	// result payload. For a long session that is megabytes per read and grows
	// linearly. When only the header is needed — the common case when resuming,
	// where Version and State are what matter — use GetHeader instead, and page
	// through history with Steps.
	Get(ctx context.Context, id uuid.UUID) (*Session, error)
	// GetHeader retrieves a session by ID WITHOUT its step history: History is
	// left nil. Cost is independent of session length. Same not-found and
	// soft-delete semantics as Get.
	//
	// A header-loaded session is safe to run steps against — step indices come
	// from the database, not from len(History) — so this is the cheap resume
	// path for a long-lived session.
	//
	// It does change what the MODEL sees: generation context is built from
	// Session.History, so a step run on a header-loaded session is sent no
	// prior turns. That is correct when the caller supplies context itself
	// (via StepRequest.Inputs, State.Snapshot/Vars, or a retrieval hook), and
	// wrong if it expects loom to carry the conversation. Use Get, or page
	// history in with Steps, when the transcript is the context.
	GetHeader(ctx context.Context, id uuid.UUID) (*Session, error)
	// GetIncludingDeleted retrieves a session by ID even if it has been
	// soft-deleted, with history loaded. Check Session.DeletedAt to tell which.
	// Note that Update rejects a soft-deleted row, so a session read this way
	// cannot be written until it is restored.
	GetIncludingDeleted(ctx context.Context, id uuid.UUID) (*Session, error)
	// Steps returns a page of a session's steps ordered by step index. A limit
	// of 0 or less means "no limit". Use this instead of Get when history is
	// large or only a window is needed.
	Steps(ctx context.Context, id uuid.UUID, limit, offset int) ([]Step, error)
	// StateAt returns the checkpointed State as of the newest step at or before
	// stepIndex, and whether such a checkpoint exists.
	//
	// Checkpoints are written inside the step transaction, so a step can never
	// commit without one. The loom_sessions row, by contrast, is updated in a
	// separate write afterwards. If that write fails (see ErrSessionNotPersisted)
	// or the process dies between the two, sessions.state is stale while the
	// checkpoint holds the truth — StateAt is how a caller recovers it.
	StateAt(ctx context.Context, id uuid.UUID, stepIndex int) (State, bool, error)
	// Update persists state changes to an existing session. Fails with
	// ErrSessionConflict if another writer advanced the row first, and with
	// *NotFoundError if the session is missing or soft-deleted.
	Update(ctx context.Context, s *Session) error
	// Fork creates a branch session from parentID at the given step index.
	//
	// The branch starts with EMPTY history: parent steps are NOT copied, and
	// nothing stitches them on at read time. Because generation context is built
	// from Session.History, a freshly forked branch also starts with no model
	// context.
	//
	// State IS rewound — the branch's State is reconstructed from the parent's
	// checkpoint at-or-before stepIndex (falling back to the parent's current
	// state if no checkpoint that early exists). So anything a caller needs to
	// survive a fork must live in State (Vars/Snapshot), not in step history.
	//
	// Step indices on the branch restart at 0; they are only unique per session,
	// so join on (session_id, step_index) and never on step_index alone.
	Fork(ctx context.Context, parentID uuid.UUID, stepIndex int) (*Session, error)
	// BranchTree returns the full descendant tree rooted at sessionID, in a
	// single query. Nodes carry session headers plus StepCount; History is not
	// loaded. Soft-deleted sessions and their subtrees are excluded.
	BranchTree(ctx context.Context, rootID uuid.UUID) (*BranchNode, error)
	// Ancestry returns the fork chain from the root down to id inclusive,
	// oldest first, as headers (History is not loaded). The last element is id
	// itself; a session with no parent returns just itself.
	//
	// Where BranchTree walks DOWN from a root and returns everything, Ancestry
	// walks UP from a leaf and returns one line — which is what a caller that
	// forks per turn needs, since a playthrough is a path through the tree and
	// the tree also holds every abandoned branch. It is one query rather than
	// one GetHeader per ancestor.
	//
	// A soft-deleted session anywhere in the chain truncates the result there:
	// the walk stops rather than reporting a lineage it cannot substantiate.
	Ancestry(ctx context.Context, id uuid.UUID) ([]*Session, error)
	// Pin marks a session as exempt from garbage collection. A pinned session is
	// skipped by all four GC tiers. It does NOT protect against the explicit
	// operator actions Discard and Purge — Purge refuses a pinned session (see
	// ErrSessionPinned), but Discard does not.
	Pin(ctx context.Context, id uuid.UUID) error
	// Unpin removes the GC exemption.
	Unpin(ctx context.Context, id uuid.UUID) error
	// Discard soft-deletes a session immediately (bypasses T1/T2 GC tiers).
	// After this the session is invisible to Get, List and BranchTree, and
	// Update rejects it; the row and its steps survive until GC or Purge.
	Discard(ctx context.Context, id uuid.UUID) error
	// Purge performs an immediate hard delete of the session and its cascaded
	// steps, actions and checkpoints. Irreversible.
	//
	// Returns ErrSessionPinned if the session is pinned — deleting pinned data
	// is usually a mistake. Use ForcePurge to delete it anyway.
	Purge(ctx context.Context, id uuid.UUID) error
	// ForcePurge is Purge without the pinned check.
	ForcePurge(ctx context.Context, id uuid.UUID) error
	// List returns sessions for a platform ID.
	List(ctx context.Context, platformID string, limit, offset int) ([]*Session, error)
}
