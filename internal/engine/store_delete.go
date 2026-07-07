package engine

import (
	"context"
	"database/sql"
	"fmt"
)

// -----------------------------------------------------------------------
// Deletes (agents, prompts, response formats)
// -----------------------------------------------------------------------

func sqlDeleteAgent(ctx context.Context, db *sql.DB, prefix, slug string, version int) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %sagents WHERE slug=$1 AND version=$2`, prefix), slug, version)
	return err
}

func sqlDeletePrompt(ctx context.Context, db *sql.DB, prefix, slug string, version int) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %sprompts WHERE slug=$1 AND version=$2`, prefix), slug, version)
	return err
}

func sqlDeleteResponseFormat(ctx context.Context, db *sql.DB, prefix, slug string, version int) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %sresponse_formats WHERE slug=$1 AND version=$2`, prefix), slug, version)
	return err
}
