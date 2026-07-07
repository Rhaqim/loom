package loom

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

func sqlQuerySession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Session, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT id, platform_id, parent_session_id, branch_point, version,
		       state, metadata, tags, pinned, deleted_at, created_at, updated_at
		FROM %ssessions WHERE id=$1`, prefix), id)
	return scanSession(row)
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
		SET state=$2, metadata=$3, tags=$4, updated_at=$5, version=version+1
		WHERE id=$1 AND version=$6 AND deleted_at IS NULL`, prefix),
		s.ID, stateJSON, metaJSON, tagsJSON, s.UpdatedAt, s.Version,
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

func sqlSetPinned(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, pinned bool) error {
	_, err := db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %ssessions SET pinned=$2, updated_at=$3 WHERE id=$1`, prefix),
		id, pinned, time.Now())
	return err
}

func sqlSoftDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	now := time.Now()
	_, err := db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %ssessions SET deleted_at=$2, updated_at=$3 WHERE id=$1`, prefix),
		id, now, now)
	return err
}

func sqlHardDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	_, err := db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %ssessions WHERE id=$1`, prefix), id)
	return err
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
