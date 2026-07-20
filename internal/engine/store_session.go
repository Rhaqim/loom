package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Session persistence
// -----------------------------------------------------------------------

func sqlInsertSession(ctx context.Context, db *sql.DB, prefix string, s *Session) error {
	stateJSON, _ := json.Marshal(s.State)
	metaJSON, _ := json.Marshal(s.Metadata)
	tagsJSON, _ := json.Marshal(s.Tags)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %ssessions
			(id, platform_id, parent_session_id, branch_point, version,
			 state, metadata, tags, pinned, deleted_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, prefix),
		s.ID, s.PlatformID, nullableUUID(s.ParentSessionID), s.BranchPoint, s.Version,
		stateJSON, metaJSON, tagsJSON, s.Pinned, s.DeletedAt,
		s.CreatedAt, s.UpdatedAt,
	)
	return err
}

// sessionColumns is the column list shared by every single-session read.
const sessionColumns = `id, platform_id, parent_session_id, branch_point, version,
		       state, metadata, tags, pinned, deleted_at, created_at, updated_at`

// sqlQuerySession reads a live session. Soft-deleted rows are excluded so reads
// agree with sqlUpdateSession, sqlListSessions and buildChildren, all of which
// filter on deleted_at — otherwise a discarded session is readable and forkable
// but not writable, and Discard fails to hide the data it was asked to remove.
func sqlQuerySession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Session, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %ssessions WHERE id=$1 AND deleted_at IS NULL`, sessionColumns, prefix), id)
	return scanSession(row)
}

// sqlQuerySessionIncludingDeleted reads a session whether or not it has been
// soft-deleted. Only for the explicit restore/audit path (GetIncludingDeleted).
func sqlQuerySessionIncludingDeleted(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Session, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %ssessions WHERE id=$1`, sessionColumns, prefix), id)
	return scanSession(row)
}

// sqlIsSessionPinned reports whether a live session is pinned. Returns
// sql.ErrNoRows if the session is missing or already discarded.
func sqlIsSessionPinned(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (bool, error) {
	var pinned bool
	err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT pinned FROM %ssessions WHERE id=$1`, prefix), id).Scan(&pinned)
	return pinned, err
}

func sqlUpdateSession(ctx context.Context, db *sql.DB, prefix string, s *Session) error {
	stateJSON, _ := json.Marshal(s.State)
	metaJSON, _ := json.Marshal(s.Metadata)
	tagsJSON, _ := json.Marshal(s.Tags)
	// Compare-and-set on version so a stale writer cannot silently overwrite a
	// newer state. On success the row's version is bumped and mirrored in memory.
	// pinned is owned exclusively by Pin/Unpin and deliberately not written here,
	// so a state Update from a stale copy cannot clobber the pin flag. Soft-deleted
	// rows are excluded so Updates do not keep mutating a discarded session.
	res, err := db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %ssessions
		SET state=$1, metadata=$2, tags=$3, updated_at=$4, version=version+1
		WHERE id=$5 AND version=$6 AND deleted_at IS NULL`, prefix),
		stateJSON, metaJSON, tagsJSON, s.UpdatedAt, s.ID, s.Version,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Distinguish a version conflict from an absent or soft-deleted row.
		var cnt int
		if qerr := db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM %ssessions WHERE id=$1 AND deleted_at IS NULL`, prefix),
			s.ID).Scan(&cnt); qerr != nil {
			return qerr
		}
		if cnt == 0 {
			return ErrNotFound
		}
		return ErrSessionConflict
	}
	s.Version++
	return nil
}

func sqlListSessions(ctx context.Context, db *sql.DB, prefix, platformID string, limit, offset int) ([]*Session, error) {
	q := fmt.Sprintf(`
		SELECT id, platform_id, parent_session_id, branch_point, version,
		       state, metadata, tags, pinned, deleted_at, created_at, updated_at
		FROM %ssessions WHERE platform_id=$1 AND deleted_at IS NULL
		ORDER BY created_at DESC LIMIT $2 OFFSET $3`, prefix)
	rows, err := db.QueryContext(ctx, q, platformID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []*Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func sqlBuildBranchTree(ctx context.Context, db *sql.DB, prefix string, rootID uuid.UUID) (*BranchNode, error) {
	root, err := sqlQuerySession(ctx, db, prefix, rootID)
	if err != nil {
		return nil, err
	}
	node := &BranchNode{Session: root}
	if err := buildChildren(ctx, db, prefix, node); err != nil {
		return nil, err
	}
	return node, nil
}

func buildChildren(ctx context.Context, db *sql.DB, prefix string, node *BranchNode) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, platform_id, parent_session_id, branch_point, version,
		       state, metadata, tags, pinned, deleted_at, created_at, updated_at
		FROM %ssessions WHERE parent_session_id=$1 AND deleted_at IS NULL
		ORDER BY created_at`, prefix), node.Session.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		child, err := scanSession(rows)
		if err != nil {
			return err
		}
		childNode := &BranchNode{Session: child}
		if err := buildChildren(ctx, db, prefix, childNode); err != nil {
			return err
		}
		node.Children = append(node.Children, childNode)
	}
	return rows.Err()
}

// Placeholders below are numbered in ascending order of APPEARANCE, not by
// logical importance, and args are appended to match. loom's own drivers
// (modernc.org/sqlite, lib/pq) bind $N by number, so this is not required for
// correctness here — but mattn/go-sqlite3 binds $N by order of appearance, and
// callers supply their own *sql.DB. Keeping appearance order == numeric order
// makes these statements correct under positional binders too. Preserve it.

func sqlSetPinned(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, pinned bool) error {
	res, err := db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %ssessions SET pinned=$1, updated_at=$2 WHERE id=$3 AND deleted_at IS NULL`, prefix),
		pinned, time.Now(), id)
	if err != nil {
		return err
	}
	return affectedOrNotFound(res, "session", id)
}

func sqlSoftDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	now := time.Now()
	res, err := db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %ssessions SET deleted_at=$1, updated_at=$2 WHERE id=$3 AND deleted_at IS NULL`, prefix),
		now, now, id)
	if err != nil {
		return err
	}
	return affectedOrNotFound(res, "session", id)
}

func sqlHardDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	res, err := db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %ssessions WHERE id=$1`, prefix), id)
	if err != nil {
		return err
	}
	return affectedOrNotFound(res, "session", id)
}

// affectedOrNotFound turns a zero-row write into *NotFoundError. Without this a
// write against a missing (or already-discarded) row reports success, which is
// the same silent-no-op failure mode as a mis-bound placeholder. Drivers that
// cannot report RowsAffected are treated as success, since we cannot tell.
func affectedOrNotFound(res sql.Result, kind string, id uuid.UUID) error {
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return &NotFoundError{Kind: kind, Key: id.String()}
	}
	return nil
}

type sessionRow interface {
	Scan(dest ...any) error
}

func scanSession(row sessionRow) (*Session, error) {
	var (
		s                             Session
		parentSessionID               sql.NullString
		branchPoint                   sql.NullInt64
		stateJSON, metaJSON, tagsJSON []byte
		deletedAt                     sql.NullTime
	)
	err := row.Scan(
		&s.ID, &s.PlatformID, &parentSessionID, &branchPoint, &s.Version,
		&stateJSON, &metaJSON, &tagsJSON, &s.Pinned, &deletedAt,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if parentSessionID.Valid {
		id, _ := uuid.Parse(parentSessionID.String)
		s.ParentSessionID = &id
	}
	if branchPoint.Valid {
		bp := int(branchPoint.Int64)
		s.BranchPoint = &bp
	}
	if deletedAt.Valid {
		s.DeletedAt = &deletedAt.Time
	}
	_ = json.Unmarshal(stateJSON, &s.State)
	_ = json.Unmarshal(metaJSON, &s.Metadata)
	_ = json.Unmarshal(tagsJSON, &s.Tags)
	return &s, nil
}
