package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
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
	// Participants are the people sharing this session (players, a host, a
	// human game master). It is part of State, so every step's checkpoint
	// records the membership at that step and a fork restores it. Change it
	// with SessionRegistry.AddParticipant / RemoveParticipant.
	Participants []Participant
}

// Participant is one person in a shared session. Role is application-defined
// ("host", "player", "dm"). ID is the application's own identifier and is what
// Action.ParticipantID and Step.ParticipantID refer to.
type Participant struct {
	ID       string
	Role     string
	JoinedAt time.Time
}

// HasParticipant reports whether id is a current participant.
func (st State) HasParticipant(id string) bool {
	for _, p := range st.Participants {
		if p.ID == id {
			return true
		}
	}
	return false
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
	cp.State.Participants = slices.Clone(s.State.Participants)
	return &cp
}

// stateBase is the State a step started from, kept so mergeState can tell
// what the step itself changed from what a concurrent sibling changed since.
// Values are held as their JSON encoding (State is persisted as JSON): a Vars
// value is `any` and only shallow-cloned by forStep, so a hook mutating a nested
// map in place would also mutate a retained copy and the change would go
// unseen. Bytes taken up front cannot be mutated that way.
type stateBase struct {
	vars         map[string][]byte
	snapshot     []byte
	actions      []byte
	participants []byte
}

// baseOf records st as a step's starting point.
func baseOf(st State) stateBase {
	b := stateBase{vars: make(map[string][]byte, len(st.Vars)), snapshot: bytes.Clone(st.Snapshot)}
	for k, v := range st.Vars {
		b.vars[k] = encodeForCompare(v)
	}
	b.actions = encodeForCompare(st.AvailableActions)
	b.participants = encodeForCompare(st.Participants)
	return b
}

// encodeForCompare returns v's JSON encoding, or nil when it has none. A nil
// encoding never equals another value's, so an unencodable value always counts
// as changed (the pre-merge behaviour: copy it).
func encodeForCompare(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func sameEncoding(a, b []byte) bool { return a != nil && b != nil && bytes.Equal(a, b) }

// mergeState reconciles a step's (possibly hook-mutated) State back onto dst
// with a three-way merge against base, the State the step started from. Only
// what the step itself changed is applied:
//
//   - Vars: each key whose value differs from base is set; a key the step
//     deleted is deleted. Keys the step left alone are not touched, so a
//     sibling's newer write to them survives.
//   - Snapshot, AvailableActions and Participants are whole values: replaced
//     only when the step changed them.
//
// This matters when RunTurn runs followers concurrently. The old two-way merge
// compared the step's copy with dst's CURRENT value, so a follower that wrote
// nothing still "differed" from a sibling's newer write and reverted it with
// its stale copy. When two steps change the same key or field, last writer
// wins; the returned names list those conflicts so the caller can log them.
func mergeState(dst *State, base stateBase, src State) (conflicts []string) {
	for k, v := range src.Vars {
		enc := encodeForCompare(v)
		was, existed := base.vars[k]
		if existed && sameEncoding(was, enc) {
			continue // unchanged by this step
		}
		if cur, ok := dst.Vars[k]; ok {
			curEnc := encodeForCompare(cur)
			changedByOther := !existed || !sameEncoding(curEnc, was)
			if changedByOther && !sameEncoding(curEnc, enc) {
				conflicts = append(conflicts, "Vars."+k)
			}
		}
		if dst.Vars == nil {
			dst.Vars = make(map[string]any, len(src.Vars))
		}
		dst.Vars[k] = v
	}
	for k, was := range base.vars {
		if _, kept := src.Vars[k]; kept {
			continue
		}
		if cur, ok := dst.Vars[k]; ok {
			if !sameEncoding(encodeForCompare(cur), was) {
				conflicts = append(conflicts, "Vars."+k)
			}
			delete(dst.Vars, k)
		}
	}
	if !bytes.Equal(src.Snapshot, base.snapshot) {
		if !bytes.Equal(dst.Snapshot, base.snapshot) && !bytes.Equal(dst.Snapshot, src.Snapshot) {
			conflicts = append(conflicts, "Snapshot")
		}
		dst.Snapshot = src.Snapshot
	}
	if srcActs := encodeForCompare(src.AvailableActions); !sameEncoding(srcActs, base.actions) {
		dstActs := encodeForCompare(dst.AvailableActions)
		if !sameEncoding(dstActs, base.actions) && !sameEncoding(dstActs, srcActs) {
			conflicts = append(conflicts, "AvailableActions")
		}
		dst.AvailableActions = src.AvailableActions
	}
	if srcP := encodeForCompare(src.Participants); !sameEncoding(srcP, base.participants) {
		dstP := encodeForCompare(dst.Participants)
		if !sameEncoding(dstP, base.participants) && !sameEncoding(dstP, srcP) {
			conflicts = append(conflicts, "Participants")
		}
		dst.Participants = src.Participants
	}
	return conflicts
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
	// AddParticipant adds p to the session's participants and persists the
	// session. JoinedAt defaults to now. Adding an ID that is already present
	// returns ErrParticipantExists. Pass the same *Session the app runs steps
	// on: the change is serialised with in-flight steps, so it is neither lost
	// nor reported as a conflict. Membership is part of State, so the next
	// step's checkpoint records it and a fork from before the join drops it.
	AddParticipant(ctx context.Context, s *Session, p Participant) error
	// RemoveParticipant removes the participant with id and persists the
	// session; an unknown id returns ErrNotParticipant.
	RemoveParticipant(ctx context.Context, s *Session, id string) error
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
