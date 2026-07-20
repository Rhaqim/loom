package engine

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Persistence wrappers
// -----------------------------------------------------------------------

// These thin wrappers delegate to the SQL functions in the store_*.go files.
// They centralise cross-cutting concerns — currently translating a bare
// ErrNotFound into a keyed *NotFoundError — so the services and engine stay
// decoupled from the raw SQL layer.

func queryAgent(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) (*Agent, error) {
	a, err := sqlQueryAgent(ctx, db, prefix, owner, slug, version)
	return a, wrapNotFound(err, "agent", fmt.Sprintf("%s@v%d", slug, version))
}
func queryAgentLatest(ctx context.Context, db *sql.DB, prefix, owner, slug string) (*Agent, error) {
	a, err := sqlQueryAgentLatest(ctx, db, prefix, owner, slug)
	return a, wrapNotFound(err, "agent", slug)
}
func queryAgentByID(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Agent, error) {
	a, err := sqlQueryAgentByID(ctx, db, prefix, id)
	return a, wrapNotFound(err, "agent", id.String())
}
func insertAgent(ctx context.Context, db *sql.DB, prefix string, a *Agent) error {
	return sqlInsertAgent(ctx, db, prefix, a)
}
func listAgents(ctx context.Context, db *sql.DB, prefix, owner, category string) ([]*Agent, error) {
	return sqlListAgents(ctx, db, prefix, owner, category)
}
func queryAgentVersions(ctx context.Context, db *sql.DB, prefix, owner, slug string) ([]*Agent, error) {
	return sqlQueryAgentVersions(ctx, db, prefix, owner, slug)
}

func queryPrompt(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) (*Prompt, error) {
	p, err := sqlQueryPrompt(ctx, db, prefix, owner, slug, version)
	return p, wrapNotFound(err, "prompt", fmt.Sprintf("%s@v%d", slug, version))
}
func queryPromptLatest(ctx context.Context, db *sql.DB, prefix, owner, slug string) (*Prompt, error) {
	p, err := sqlQueryPromptLatest(ctx, db, prefix, owner, slug)
	return p, wrapNotFound(err, "prompt", slug)
}
func queryPromptByID(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Prompt, error) {
	p, err := sqlQueryPromptByID(ctx, db, prefix, id)
	return p, wrapNotFound(err, "prompt", id.String())
}
func insertPrompt(ctx context.Context, db *sql.DB, prefix string, p *Prompt) error {
	return sqlInsertPrompt(ctx, db, prefix, p)
}
func listPrompts(ctx context.Context, db *sql.DB, prefix, owner string, kind PromptKind, category string) ([]*Prompt, error) {
	return sqlListPrompts(ctx, db, prefix, owner, kind, category)
}
func queryPromptVersions(ctx context.Context, db *sql.DB, prefix, owner, slug string) ([]*Prompt, error) {
	return sqlQueryPromptVersions(ctx, db, prefix, owner, slug)
}

func querySession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Session, error) {
	s, err := sqlQuerySession(ctx, db, prefix, id)
	return s, wrapNotFound(err, "session", id.String())
}
func querySessionIncludingDeleted(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Session, error) {
	s, err := sqlQuerySessionIncludingDeleted(ctx, db, prefix, id)
	return s, wrapNotFound(err, "session", id.String())
}
func isSessionPinned(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (bool, error) {
	p, err := sqlIsSessionPinned(ctx, db, prefix, id)
	return p, wrapNotFound(err, "session", id.String())
}
func insertSession(ctx context.Context, db *sql.DB, prefix string, s *Session) error {
	return sqlInsertSession(ctx, db, prefix, s)
}
func updateSession(ctx context.Context, db *sql.DB, prefix string, s *Session) error {
	return sqlUpdateSession(ctx, db, prefix, s)
}
func buildBranchTree(ctx context.Context, db *sql.DB, prefix string, rootID uuid.UUID) (*BranchNode, error) {
	return sqlBuildBranchTree(ctx, db, prefix, rootID)
}
func setPinned(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, pinned bool) error {
	return sqlSetPinned(ctx, db, prefix, id, pinned)
}
func softDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	return sqlSoftDeleteSession(ctx, db, prefix, id)
}
func hardDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	return sqlHardDeleteSession(ctx, db, prefix, id)
}
func listSessions(ctx context.Context, db *sql.DB, prefix, platformID string, limit, offset int) ([]*Session, error) {
	return sqlListSessions(ctx, db, prefix, platformID, limit, offset)
}

func querySteps(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID) ([]Step, error) {
	return sqlQuerySteps(ctx, db, prefix, sessionID)
}
func queryStepsPage(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID, limit, offset int) ([]Step, error) {
	return sqlQueryStepsPage(ctx, db, prefix, sessionID, limit, offset)
}
func insertStep(ctx context.Context, db *sql.DB, prefix string, step *Step, checkpoint *State) error {
	return sqlInsertStep(ctx, db, prefix, step, checkpoint)
}
func querySnapshotAt(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID, stepIndex int) (State, bool, error) {
	return sqlQuerySnapshotAt(ctx, db, prefix, sessionID, stepIndex)
}

// JudgeRegistry is the public interface returned by Engine.Judges().
