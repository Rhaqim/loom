package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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

// maxLineageDepth bounds the recursive session walks. parent_session_id should
// never form a cycle, but neither dialect detects one by default, so a single
// corrupt row would spin a recursive CTE forever and hang the caller. A story
// deeper than this is already pathological.
const maxLineageDepth = 10000

// sqlBuildBranchTree loads the whole descendant tree in ONE query and assembles
// it in memory. The previous implementation issued a query per node, so
// rendering a branch tree cost N round trips — and a rewind-heavy session, the
// case that grows a tree in the first place, paid the most.
//
// Each row also carries its step count, because BranchTree reads session rows
// only: a node's History is never populated, so len(History) is not a count.
func sqlBuildBranchTree(ctx context.Context, db *sql.DB, prefix string, rootID uuid.UUID) (*BranchNode, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		WITH RECURSIVE tree(id, depth) AS (
			SELECT id, 0 FROM %[1]ssessions WHERE id=$1 AND deleted_at IS NULL
			UNION ALL
			SELECT s.id, tree.depth+1
			FROM %[1]ssessions s
			JOIN tree ON s.parent_session_id = tree.id
			WHERE s.deleted_at IS NULL AND tree.depth < %[2]d
		)
		SELECT %[3]s,
		       (SELECT COUNT(*) FROM %[1]ssteps st WHERE st.session_id = se.id)
		FROM tree
		JOIN %[1]ssessions se ON se.id = tree.id
		ORDER BY tree.depth, se.created_at`, prefix, maxLineageDepth, prefixedSessionColumns("se")),
		rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := map[uuid.UUID]*BranchNode{}
	// Ordered by depth then created_at, so a node's parent is always seen
	// before the node itself and children attach in creation order.
	var order []*BranchNode
	for rows.Next() {
		s, count, err := scanSessionWithCount(rows)
		if err != nil {
			return nil, err
		}
		n := &BranchNode{Session: s, StepCount: count}
		nodes[s.ID] = n
		order = append(order, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return nil, &NotFoundError{Kind: "session", Key: rootID.String()}
	}
	for _, n := range order[1:] {
		if n.Session.ParentSessionID == nil {
			continue
		}
		if parent, ok := nodes[*n.Session.ParentSessionID]; ok {
			parent.Children = append(parent.Children, n)
		}
	}
	return order[0], nil
}

// sqlQueryAncestry returns the chain from the root down to id inclusive, oldest
// first, as headers (no step history).
//
// A fork chain is a PATH through the branch tree, and BranchTree only walks
// downward from a root — so reading one playthrough previously meant following
// ParentSessionID upward one query at a time: N round trips on a read path that
// grows with every turn. This walks it in one query on both dialects.
func sqlQueryAncestry(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) ([]*Session, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		WITH RECURSIVE anc(id, parent_session_id, depth) AS (
			SELECT id, parent_session_id, 0 FROM %[1]ssessions WHERE id=$1 AND deleted_at IS NULL
			UNION ALL
			SELECT s.id, s.parent_session_id, anc.depth+1
			FROM %[1]ssessions s
			JOIN anc ON s.id = anc.parent_session_id
			WHERE s.deleted_at IS NULL AND anc.depth < %[2]d
		)
		SELECT %[3]s
		FROM anc
		JOIN %[1]ssessions se ON se.id = anc.id
		ORDER BY anc.depth DESC`, prefix, maxLineageDepth, prefixedSessionColumns("se")),
		id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, &NotFoundError{Kind: "session", Key: id.String()}
	}
	return out, nil
}

// prefixedSessionColumns qualifies the shared session column list with a table
// alias, so the same ordering can be reused inside a join without repeating the
// list and risking it drifting out of step with scanSession.
func prefixedSessionColumns(alias string) string {
	cols := strings.Split(sessionColumns, ",")
	for i, c := range cols {
		cols[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
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
	return scanSessionExtra(row)
}

// scanSessionWithCount scans a session row followed by one trailing COUNT(*)
// column, as the branch-tree query emits.
func scanSessionWithCount(row sessionRow) (*Session, int, error) {
	var count int
	s, err := scanSessionExtra(row, &count)
	return s, count, err
}

// scanSessionExtra scans the standard session columns plus any trailing
// destinations the caller's query appended, so a joined-on aggregate does not
// need a second copy of the session-scanning logic.
func scanSessionExtra(row sessionRow, extra ...any) (*Session, error) {
	var (
		s                             Session
		parentSessionID               sql.NullString
		branchPoint                   sql.NullInt64
		stateJSON, metaJSON, tagsJSON []byte
		deletedAt                     sql.NullTime
	)
	dest := []any{
		&s.ID, &s.PlatformID, &parentSessionID, &branchPoint, &s.Version,
		&stateJSON, &metaJSON, &tagsJSON, &s.Pinned, &deletedAt,
		&s.CreatedAt, &s.UpdatedAt,
	}
	dest = append(dest, extra...)
	err := row.Scan(dest...)
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
