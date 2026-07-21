package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Response-format persistence (reusable, versioned records)
// -----------------------------------------------------------------------

const responseFormatColumns = "id, slug, version, schema, strict, created_at, owner, category"

// responseFormatListColumns omits the schema body: List is an index for an
// editor, and a schema can be arbitrarily large. Callers fetch bodies with Get.
const responseFormatListColumns = "id, slug, version, strict, created_at, owner, category"

func sqlInsertResponseFormat(ctx context.Context, db *sql.DB, prefix string, rf *ResponseFormatRecord) error {
	schemaJSON, _ := json.Marshal(rf.Schema)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sresponse_formats (id, slug, version, schema, strict, created_at, owner, category)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, prefix),
		rf.ID, rf.Slug, rf.Version, schemaJSON, rf.StrictMode, rf.CreatedAt, rf.Owner, rf.Category,
	)
	return err
}

func sqlQueryResponseFormat(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) (*ResponseFormatRecord, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %sresponse_formats WHERE owner=$1 AND slug=$2 AND version=$3`, responseFormatColumns, prefix), owner, slug, version)
	return scanResponseFormat(row)
}

func sqlQueryResponseFormatLatest(ctx context.Context, db *sql.DB, prefix, owner, slug string) (*ResponseFormatRecord, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %sresponse_formats WHERE owner=$1 AND slug=$2 ORDER BY version DESC LIMIT 1`, responseFormatColumns, prefix), owner, slug)
	return scanResponseFormat(row)
}

// sqlQueryResponseFormatByID is intentionally not owner-filtered: the row UUID
// is globally unique and this resolves loom's own FK references.
func sqlQueryResponseFormatByID(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*ResponseFormatRecord, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %sresponse_formats WHERE id=$1`, responseFormatColumns, prefix), id)
	return scanResponseFormat(row)
}

func scanResponseFormat(row promptRow) (*ResponseFormatRecord, error) {
	var (
		r          ResponseFormatRecord
		schemaJSON []byte
	)
	err := row.Scan(&r.ID, &r.Slug, &r.Version, &schemaJSON, &r.StrictMode, &r.CreatedAt, &r.Owner, &r.Category)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(schemaJSON, &r.Schema)
	return &r, nil
}

// sqlListResponseFormats returns an owner's response formats without their
// schema bodies, newest version first within each slug. Placeholders appear in
// ascending order — see the note in store_session.go.
func sqlListResponseFormats(ctx context.Context, db *sql.DB, prefix, owner, category string) ([]*ResponseFormatRecord, error) {
	q := fmt.Sprintf(`SELECT %s FROM %sresponse_formats WHERE owner=$1`, responseFormatListColumns, prefix)
	args := []any{owner}
	if category != "" {
		q += " AND category=$2"
		args = append(args, category)
	}
	q += " ORDER BY slug, version DESC"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ResponseFormatRecord
	for rows.Next() {
		var r ResponseFormatRecord
		// Schema is deliberately not selected, so it stays nil.
		if err := rows.Scan(&r.ID, &r.Slug, &r.Version, &r.StrictMode,
			&r.CreatedAt, &r.Owner, &r.Category); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
