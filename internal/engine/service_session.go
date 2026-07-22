package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type sessionService struct{ e *Engine }

func (s *sessionService) Create(ctx context.Context, sess *Session) error {
	if sess.ID == uuid.Nil {
		sess.ID = uuid.New()
	}
	now := time.Now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	return insertSession(ctx, s.e.db, s.e.prefix, sess)
}

func (s *sessionService) Get(ctx context.Context, id uuid.UUID) (*Session, error) {
	sess, err := querySession(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, err
	}
	steps, err := querySteps(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, err
	}
	sess.History = steps
	return sess, nil
}

func (s *sessionService) GetHeader(ctx context.Context, id uuid.UUID) (*Session, error) {
	// Deliberately does not touch loom_steps: cost is independent of history
	// length. History is left nil rather than empty so a caller can tell "not
	// loaded" from "no steps".
	return querySession(ctx, s.e.db, s.e.prefix, id)
}

func (s *sessionService) GetIncludingDeleted(ctx context.Context, id uuid.UUID) (*Session, error) {
	sess, err := querySessionIncludingDeleted(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, err
	}
	steps, err := querySteps(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, err
	}
	sess.History = steps
	return sess, nil
}

func (s *sessionService) Steps(ctx context.Context, id uuid.UUID, limit, offset int) ([]Step, error) {
	return queryStepsPage(ctx, s.e.db, s.e.prefix, id, limit, offset)
}

func (s *sessionService) StateAt(ctx context.Context, id uuid.UUID, stepIndex int) (State, bool, error) {
	return querySnapshotAt(ctx, s.e.db, s.e.prefix, id, stepIndex)
}

func (s *sessionService) Update(ctx context.Context, sess *Session) error {
	sess.UpdatedAt = time.Now()
	return updateSession(ctx, s.e.db, s.e.prefix, sess)
}

func (s *sessionService) Fork(ctx context.Context, parentID uuid.UUID, stepIndex int) (*Session, error) {
	parent, err := querySession(ctx, s.e.db, s.e.prefix, parentID)
	if err != nil {
		return nil, fmt.Errorf("loom: fork: %w", err)
	}
	branch := &Session{
		ID:              uuid.New(),
		PlatformID:      parent.PlatformID,
		ParentSessionID: &parentID,
		BranchPoint:     &stepIndex,
		State:           parent.State,
		Tags:            append([]string(nil), parent.Tags...),
		Metadata:        parent.Metadata,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	// Reconstruct full state as of the fork point from the per-step checkpoint so
	// the branch inherits the state after stepIndex, not the parent's latest. When
	// no checkpoint exists that early (e.g. a pre-checkpoint session), fall back to
	// the parent's current state.
	if st, found, err := querySnapshotAt(ctx, s.e.db, s.e.prefix, parentID, stepIndex); err != nil {
		return nil, fmt.Errorf("loom: fork: load checkpoint: %w", err)
	} else if found {
		branch.State = st
	}
	if err := insertSession(ctx, s.e.db, s.e.prefix, branch); err != nil {
		return nil, err
	}
	return branch, nil
}

func (s *sessionService) BranchTree(ctx context.Context, rootID uuid.UUID) (*BranchNode, error) {
	return buildBranchTree(ctx, s.e.db, s.e.prefix, rootID)
}

func (s *sessionService) Ancestry(ctx context.Context, id uuid.UUID) ([]*Session, error) {
	return queryAncestry(ctx, s.e.db, s.e.prefix, id)
}

func (s *sessionService) Pin(ctx context.Context, id uuid.UUID) error {
	return setPinned(ctx, s.e.db, s.e.prefix, id, true)
}

func (s *sessionService) Unpin(ctx context.Context, id uuid.UUID) error {
	return setPinned(ctx, s.e.db, s.e.prefix, id, false)
}

func (s *sessionService) Discard(ctx context.Context, id uuid.UUID) error {
	return softDeleteSession(ctx, s.e.db, s.e.prefix, id)
}

func (s *sessionService) Purge(ctx context.Context, id uuid.UUID) error {
	// Pin means "do not collect this". GC honours it on all four tiers; an
	// explicit Purge previously did not, so a pin gave no protection against the
	// one path that deletes irreversibly. Refuse, and make the override explicit.
	// The pin lookup ignores deleted_at so a discarded-but-pinned session is
	// still protected.
	pinned, err := isSessionPinned(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return err
	}
	if pinned {
		return fmt.Errorf("%w: %s (use ForcePurge to delete anyway)", ErrSessionPinned, id)
	}
	return hardDeleteSession(ctx, s.e.db, s.e.prefix, id)
}

func (s *sessionService) ForcePurge(ctx context.Context, id uuid.UUID) error {
	return hardDeleteSession(ctx, s.e.db, s.e.prefix, id)
}

func (s *sessionService) List(ctx context.Context, platformID string, limit, offset int) ([]*Session, error) {
	return listSessions(ctx, s.e.db, s.e.prefix, platformID, limit, offset)
}

// stepService implements StepRunner and houses the core generation loop.
