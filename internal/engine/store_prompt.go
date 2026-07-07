package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Prompt persistence
// -----------------------------------------------------------------------

const promptColumns = "id, slug, version, kind, category, body, variables, metadata, created_at, notes, owner"

func sqlInsertPrompt(ctx context.Context, db *sql.DB, prefix string, p *Prompt) error {
	varsJSON, _ := json.Marshal(p.Variables)
	metaJSON, _ := json.Marshal(p.Metadata)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sprompts
			(id, slug, version, kind, category, body, variables, metadata, created_at, notes, owner)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, prefix),
		p.ID, p.Slug, p.Version, string(p.Kind), p.Category,
		p.Body, varsJSON, metaJSON, p.CreatedAt, p.Notes, p.Owner,
	)
	return err
}

func sqlQueryPrompt(ctx context.Context, db *sql.DB, prefix, slug string, version int) (*Prompt, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %sprompts WHERE slug=$1 AND version=$2`, promptColumns, prefix), slug, version)
	return scanPrompt(row)
}

func sqlQueryPromptLatest(ctx context.Context, db *sql.DB, prefix, slug string) (*Prompt, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %sprompts WHERE slug=$1 ORDER BY version DESC LIMIT 1`, promptColumns, prefix), slug)
	return scanPrompt(row)
}

func sqlQueryPromptByID(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Prompt, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %sprompts WHERE id=$1`, promptColumns, prefix), id)
	return scanPrompt(row)
}

func sqlListPrompts(ctx context.Context, db *sql.DB, prefix string, kind PromptKind, category string) ([]*Prompt, error) {
	args := []any{}
	conds := []string{}
	if kind != "" {
		args = append(args, string(kind))
		conds = append(conds, fmt.Sprintf("kind=$%d", len(args)))
	}
	if category != "" {
		args = append(args, category)
		conds = append(conds, fmt.Sprintf("category=$%d", len(args)))
	}
	q := fmt.Sprintf("SELECT %s FROM %sprompts", promptColumns, prefix)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY slug, version"
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prompts []*Prompt
	for rows.Next() {
		p, err := scanPrompt(rows)
		if err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

type promptRow interface {
	Scan(dest ...any) error
}

func scanPrompt(row promptRow) (*Prompt, error) {
	var (
		p                  Prompt
		kind               string
		varsJSON, metaJSON []byte
	)
	err := row.Scan(&p.ID, &p.Slug, &p.Version, &kind, &p.Category,
		&p.Body, &varsJSON, &metaJSON, &p.CreatedAt, &p.Notes, &p.Owner)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Kind = PromptKind(kind)
	_ = json.Unmarshal(varsJSON, &p.Variables)
	_ = json.Unmarshal(metaJSON, &p.Metadata)
	return &p, nil
}
