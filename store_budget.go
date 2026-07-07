package loom

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Budget persistence
// -----------------------------------------------------------------------

func sqlInsertBudget(ctx context.Context, db *sql.DB, prefix string, b *Budget) error {
	tagsIncJSON, _ := json.Marshal(b.TagsInclude)
	tagsExcJSON, _ := json.Marshal(b.TagsExclude)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sbudgets
			(id, name, target_kind, target_key, "window",
			 limit_usd, limit_tokens, limit_steps, on_exceed,
			 tags_include, tags_exclude, active, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, prefix),
		b.ID, b.Name, b.Target.Kind, b.Target.Key, string(b.Window),
		nullFloat(b.Limit.USD), nullInt(b.Limit.Tokens), nullInt(b.Limit.Steps),
		string(b.OnExceed), tagsIncJSON, tagsExcJSON, b.Active, b.CreatedAt,
	)
	return err
}

func sqlQueryBudget(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Budget, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT id, name, target_kind, target_key, "window",
		       limit_usd, limit_tokens, limit_steps, on_exceed,
		       tags_include, tags_exclude, active, created_at
		FROM %sbudgets WHERE id=$1`, prefix), id)
	return scanBudget(row)
}

func sqlListBudgets(ctx context.Context, db *sql.DB, prefix, targetKind, targetKey string) ([]*Budget, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, name, target_kind, target_key, "window",
		       limit_usd, limit_tokens, limit_steps, on_exceed,
		       tags_include, tags_exclude, active, created_at
		FROM %sbudgets WHERE target_kind=$1 AND target_key=$2 AND active=true`, prefix),
		targetKind, targetKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var budgets []*Budget
	for rows.Next() {
		b, err := scanBudget(rows)
		if err != nil {
			return nil, err
		}
		budgets = append(budgets, b)
	}
	return budgets, rows.Err()
}

func sqlDeleteBudget(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %sbudgets WHERE id=$1`, prefix), id)
	return err
}

type budgetRow interface {
	Scan(dest ...any) error
}

func scanBudget(row budgetRow) (*Budget, error) {
	var (
		b                        Budget
		window, onExceed         string
		limitUSD                 sql.NullFloat64
		limitTokens, limitSteps  sql.NullInt64
		tagsIncJSON, tagsExcJSON []byte
	)
	err := row.Scan(
		&b.ID, &b.Name, &b.Target.Kind, &b.Target.Key, &window,
		&limitUSD, &limitTokens, &limitSteps, &onExceed,
		&tagsIncJSON, &tagsExcJSON, &b.Active, &b.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.Window = BudgetWindow(window)
	b.OnExceed = BudgetAction(onExceed)
	if limitUSD.Valid {
		b.Limit.USD = limitUSD.Float64
	}
	if limitTokens.Valid {
		b.Limit.Tokens = int(limitTokens.Int64)
	}
	if limitSteps.Valid {
		b.Limit.Steps = int(limitSteps.Int64)
	}
	_ = json.Unmarshal(tagsIncJSON, &b.TagsInclude)
	_ = json.Unmarshal(tagsExcJSON, &b.TagsExclude)
	return &b, nil
}
