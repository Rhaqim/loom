package loom

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
	return hardDeleteSession(ctx, s.e.db, s.e.prefix, id)
}

func (s *sessionService) List(ctx context.Context, platformID string, limit, offset int) ([]*Session, error) {
	return listSessions(ctx, s.e.db, s.e.prefix, platformID, limit, offset)
}

// stepService implements StepRunner and houses the core generation loop.
