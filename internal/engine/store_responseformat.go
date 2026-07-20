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

const responseFormatColumns = "id, slug, version, schema, strict, created_at, owner"

func sqlInsertResponseFormat(ctx context.Context, db *sql.DB, prefix string, rf *ResponseFormatRecord) error {
	schemaJSON, _ := json.Marshal(rf.Schema)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sresponse_formats (id, slug, version, schema, strict, created_at, owner)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, prefix),
		rf.ID, rf.Slug, rf.Version, schemaJSON, rf.StrictMode, rf.CreatedAt, rf.Owner,
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
	err := row.Scan(&r.ID, &r.Slug, &r.Version, &schemaJSON, &r.StrictMode, &r.CreatedAt, &r.Owner)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(schemaJSON, &r.Schema)
	return &r, nil
}
