package loom

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Session is a running playthrough. It holds state, action log, result history,
// and any custom data the platform attaches.
type Session struct {
	ID              uuid.UUID
	PlatformID      string     // opaque — set by the platform (user ID, story ID, etc.)
	ParentSessionID *uuid.UUID // non-nil for branch sessions
	BranchPoint     *int       // step index the branch forks from
	State           State
	History         []Step
	Tags            []string
	Pinned          bool
	DeletedAt       *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Metadata        map[string]any // platform-controlled custom data
}

// State is the live state of a session between steps.
type State struct {
	Modality         Modality       // current dominant modality (the latest result kind)
	Snapshot         []byte         // serialized application state (opaque to engine)
	Vars             map[string]any // engine-tracked typed variables
	AvailableActions []ActionTemplate
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
	// Get retrieves a session by ID.
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
