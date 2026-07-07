package loom

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Stub persistence functions — implemented fully in internal/store/postgres
// -----------------------------------------------------------------------

// These thin wrappers delegate to the SQL functions defined in persist.go.
// They are kept here to keep engine.go self-contained; the actual SQL lives
// in persist.go so this file stays readable.

func queryAgent(ctx context.Context, db *sql.DB, prefix, slug string, version int) (*Agent, error) {
	return sqlQueryAgent(ctx, db, prefix, slug, version)
}
func queryAgentLatest(ctx context.Context, db *sql.DB, prefix, slug string) (*Agent, error) {
	return sqlQueryAgentLatest(ctx, db, prefix, slug)
}
func insertAgent(ctx context.Context, db *sql.DB, prefix string, a *Agent) error {
	return sqlInsertAgent(ctx, db, prefix, a)
}
func listAgents(ctx context.Context, db *sql.DB, prefix, category string) ([]*Agent, error) {
	return sqlListAgents(ctx, db, prefix, category)
}

func queryPrompt(ctx context.Context, db *sql.DB, prefix, slug string, version int) (*Prompt, error) {
	return sqlQueryPrompt(ctx, db, prefix, slug, version)
}
func queryPromptLatest(ctx context.Context, db *sql.DB, prefix, slug string) (*Prompt, error) {
	return sqlQueryPromptLatest(ctx, db, prefix, slug)
}
func queryPromptByID(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Prompt, error) {
	return sqlQueryPromptByID(ctx, db, prefix, id)
}
func insertPrompt(ctx context.Context, db *sql.DB, prefix string, p *Prompt) error {
	return sqlInsertPrompt(ctx, db, prefix, p)
}
func listPrompts(ctx context.Context, db *sql.DB, prefix string, kind PromptKind, category string) ([]*Prompt, error) {
	return sqlListPrompts(ctx, db, prefix, kind, category)
}

func querySession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Session, error) {
	return sqlQuerySession(ctx, db, prefix, id)
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
func insertStep(ctx context.Context, db *sql.DB, prefix string, step *Step, checkpoint *State) error {
	return sqlInsertStep(ctx, db, prefix, step, checkpoint)
}
func querySnapshotAt(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID, stepIndex int) (State, bool, error) {
	return sqlQuerySnapshotAt(ctx, db, prefix, sessionID, stepIndex)
}

// JudgeRegistry is the public interface returned by Engine.Judges().
