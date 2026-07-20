package engine

import (
	"context"
	"database/sql"
	"fmt"
)

// -----------------------------------------------------------------------
// Deletes (agents, prompts, response formats)
// -----------------------------------------------------------------------

func sqlDeleteAgent(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %sagents WHERE owner=$1 AND slug=$2 AND version=$3`, prefix), owner, slug, version)
	return err
}

func sqlDeletePrompt(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %sprompts WHERE owner=$1 AND slug=$2 AND version=$3`, prefix), owner, slug, version)
	return err
}

func sqlDeleteResponseFormat(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %sresponse_formats WHERE owner=$1 AND slug=$2 AND version=$3`, prefix), owner, slug, version)
	return err
}
