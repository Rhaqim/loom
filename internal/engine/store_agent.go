package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Agent persistence
// -----------------------------------------------------------------------

const agentColumns = `id, slug, version, category, modality, generator_slug,
	system_prompt_id, user_template_id, response_format_id, response_format, params,
	fallback_agent_id, created_at, owner`

func sqlInsertAgent(ctx context.Context, db *sql.DB, prefix string, a *Agent) error {
	rfJSON, _ := json.Marshal(a.ResponseFormat)
	paramsJSON, _ := json.Marshal(a.Params)
	var fallbackID *string
	if a.FallbackAgentID != nil {
		s := a.FallbackAgentID.String()
		fallbackID = &s
	}
	var rfID *string
	if a.ResponseFormatID != nil && *a.ResponseFormatID != uuid.Nil {
		s := a.ResponseFormatID.String()
		rfID = &s
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sagents
			(id, slug, version, category, modality, generator_slug,
			 system_prompt_id, user_template_id, response_format_id, response_format, params,
			 fallback_agent_id, created_at, owner)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		prefix),
		a.ID, a.Slug, a.Version, a.Category, string(a.Modal),
		a.GeneratorSlug, nullUUID(a.SystemPromptID), nullUUID(a.UserTemplateID),
		rfID, rfJSON, paramsJSON, fallbackID, a.CreatedAt, a.Owner,
	)
	return err
}

func sqlQueryAgent(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) (*Agent, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %sagents WHERE owner=$1 AND slug=$2 AND version=$3`, agentColumns, prefix), owner, slug, version)
	return scanAgent(row)
}

func sqlQueryAgentLatest(ctx context.Context, db *sql.DB, prefix, owner, slug string) (*Agent, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %sagents WHERE owner=$1 AND slug=$2 ORDER BY version DESC LIMIT 1`, agentColumns, prefix), owner, slug)
	return scanAgent(row)
}

// sqlQueryAgentByID is intentionally not owner-filtered: the row UUID is
// globally unique and this resolves loom's own FK references.
func sqlQueryAgentByID(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Agent, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %sagents WHERE id=$1`, agentColumns, prefix), id)
	return scanAgent(row)
}

func sqlListAgents(ctx context.Context, db *sql.DB, prefix, owner, category string) ([]*Agent, error) {
	q := fmt.Sprintf(`SELECT %s FROM %sagents WHERE owner=$1`, agentColumns, prefix)
	args := []any{owner}
	if category != "" {
		q += " AND category=$2"
		args = append(args, category)
	}
	q += " ORDER BY slug, version"
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func sqlQueryAgentVersions(ctx context.Context, db *sql.DB, prefix, owner, slug string) ([]*Agent, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %sagents WHERE owner=$1 AND slug=$2 ORDER BY version DESC`, agentColumns, prefix), owner, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

type agentRow interface {
	Scan(dest ...any) error
}

func scanAgent(row agentRow) (*Agent, error) {
	var (
		a                                             Agent
		rfJSON, paramsJSON                            []byte
		sysPromptID, userTemplateID, fallbackID, rfID sql.NullString
		modal                                         string
	)
	err := row.Scan(
		&a.ID, &a.Slug, &a.Version, &a.Category, &modal, &a.GeneratorSlug,
		&sysPromptID, &userTemplateID, &rfID, &rfJSON, &paramsJSON,
		&fallbackID, &a.CreatedAt, &a.Owner,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Modal = Modality(modal)
	if sysPromptID.Valid {
		id, _ := uuid.Parse(sysPromptID.String)
		a.SystemPromptID = id
	}
	if userTemplateID.Valid {
		id, _ := uuid.Parse(userTemplateID.String)
		a.UserTemplateID = id
	}
	if fallbackID.Valid {
		id, _ := uuid.Parse(fallbackID.String)
		a.FallbackAgentID = &id
	}
	if rfID.Valid && rfID.String != "" {
		id, _ := uuid.Parse(rfID.String)
		a.ResponseFormatID = &id
	}
	_ = json.Unmarshal(rfJSON, &a.ResponseFormat)
	_ = json.Unmarshal(paramsJSON, &a.Params)
	return &a, nil
}
