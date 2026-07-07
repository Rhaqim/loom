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
// Cost persistence
// -----------------------------------------------------------------------

func sqlInsertCostRecord(ctx context.Context, db *sql.DB, prefix string, rec CostRecord) error {
	tagsJSON, _ := json.Marshal(rec.Tags)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %scost_records
			(id, step_id, session_id, agent_id, platform_id,
			 provider, model, modality, input_tokens, output_tokens,
			 images, duration_sec, usd_cost, estimated, tags, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, prefix),
		uuid.New(), nullableUUID2(rec.StepID), nullableUUID2(rec.SessionID),
		nullableUUID2(rec.AgentID), rec.PlatformID,
		rec.Provider, rec.Model, string(rec.Modal),
		rec.InputTokens, rec.OutputTokens,
		rec.Images, rec.DurationSec, rec.USDCost, rec.Estimated,
		tagsJSON, rec.Timestamp,
	)
	return err
}

func sqlSessionUsage(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID) (*UsageSummary, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(usd_cost),0), COALESCE(SUM(input_tokens+output_tokens),0), COUNT(*)
		FROM %scost_records WHERE session_id=$1`, prefix), sessionID)
	s := &UsageSummary{ByModel: map[string]float64{}}
	return s, row.Scan(&s.TotalUSD, &s.TotalTokens, &s.StepCount)
}

func sqlUsage(ctx context.Context, db *sql.DB, prefix string, q UsageQuery) (*UsageSummary, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(usd_cost),0), COALESCE(SUM(input_tokens+output_tokens),0), COUNT(*)
		FROM %scost_records
		WHERE platform_id=$1 AND created_at BETWEEN $2 AND $3`, prefix),
		q.Target.Key, q.From, q.To)
	s := &UsageSummary{ByModel: map[string]float64{}}
	return s, row.Scan(&s.TotalUSD, &s.TotalTokens, &s.StepCount)
}

func sqlByAgent(ctx context.Context, db *sql.DB, prefix string, q UsageQuery) ([]AgentUsage, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT a.slug, a.version, COUNT(cr.id), COALESCE(SUM(cr.usd_cost),0)
		FROM %scost_records cr
		JOIN %sagents a ON a.id = cr.agent_id
		WHERE cr.created_at BETWEEN $1 AND $2
		GROUP BY a.slug, a.version
		ORDER BY SUM(cr.usd_cost) DESC`, prefix, prefix), q.From, q.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentUsage
	for rows.Next() {
		var u AgentUsage
		if err := rows.Scan(&u.AgentSlug, &u.AgentVersion, &u.RunCount, &u.USDTotal); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func sqlAgentStats(ctx context.Context, db *sql.DB, prefix string, agentID uuid.UUID, window time.Duration) (*AgentCostStats, error) {
	since := time.Now().Add(-window)
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT
			AVG(usd_cost),
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY usd_cost),
			PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY usd_cost),
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY input_tokens+output_tokens),
			PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY input_tokens+output_tokens)
		FROM %scost_records WHERE agent_id=$1 AND created_at >= $2`, prefix),
		agentID, since)
	s := &AgentCostStats{}
	return s, row.Scan(&s.USDMean, &s.USDP50, &s.USDP95, &s.TokensP50, &s.TokensP95)
}
