package engine

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

func sqlInsertCostRecord(ctx context.Context, db execer, prefix string, rec CostRecord) error {
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

// estimatedCountSQL counts rows priced from the placeholder rate. Written as
// SUM(CASE ...) rather than BOOL_OR/COUNT FILTER so it runs on both Postgres
// and SQLite.
const estimatedCountSQL = `COALESCE(SUM(CASE WHEN estimated THEN 1 ELSE 0 END),0)`

// sqlSessionUsage returns the session's totals and how many of the summed rows
// were priced from the placeholder rate (see costService.pricedFromRows).
func sqlSessionUsage(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID) (*UsageSummary, int, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(usd_cost),0), COALESCE(SUM(input_tokens+output_tokens),0), COUNT(*), %s
		FROM %scost_records WHERE session_id=$1`, estimatedCountSQL, prefix), sessionID)
	s := &UsageSummary{ByModel: map[string]float64{}}
	var estimated int
	err := row.Scan(&s.TotalUSD, &s.TotalTokens, &s.StepCount, &estimated)
	return s, estimated, err
}

// sqlUsage is sqlSessionUsage for a platform over a time window.
func sqlUsage(ctx context.Context, db *sql.DB, prefix string, q UsageQuery) (*UsageSummary, int, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(usd_cost),0), COALESCE(SUM(input_tokens+output_tokens),0), COUNT(*), %s
		FROM %scost_records
		WHERE platform_id=$1 AND created_at BETWEEN $2 AND $3`, estimatedCountSQL, prefix),
		q.Target.Key, q.From, q.To)
	s := &UsageSummary{ByModel: map[string]float64{}}
	var estimated int
	err := row.Scan(&s.TotalUSD, &s.TotalTokens, &s.StepCount, &estimated)
	return s, estimated, err
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

// sqlAgentStats also returns the row count and how many rows were priced from
// the placeholder rate (see costService.pricedFromRows).
func sqlAgentStats(ctx context.Context, db *sql.DB, prefix string, agentID uuid.UUID, window time.Duration) (*AgentCostStats, int, int, error) {
	since := time.Now().Add(-window)
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT
			AVG(usd_cost),
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY usd_cost),
			PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY usd_cost),
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY input_tokens+output_tokens),
			PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY input_tokens+output_tokens),
			COUNT(*),
			%s
		FROM %scost_records WHERE agent_id=$1 AND created_at >= $2`, estimatedCountSQL, prefix),
		agentID, since)
	s := &AgentCostStats{}
	var rows, estimated int
	err := row.Scan(&s.USDMean, &s.USDP50, &s.USDP95, &s.TokensP50, &s.TokensP95, &rows, &estimated)
	return s, rows, estimated, err
}

// sqlStepUsage returns one usage row per recorded step of a session, oldest
// first. Rows with a NULL step_id are skipped — those are session- or
// platform-level charges with no step to attribute them to.
//
// This lives on CostManager rather than being joined into querySteps on
// purpose: Session.Get is already the expensive read (see GetHeader/Steps), and
// folding a cost join into it would put that cost on every session load whether
// the caller wants usage or not.
func sqlStepUsage(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID) ([]StepUsage, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT step_id, agent_id, provider, model, modality,
		       input_tokens, output_tokens, images, duration_sec,
		       usd_cost, estimated, created_at
		FROM %scost_records
		WHERE session_id=$1 AND step_id IS NOT NULL
		ORDER BY created_at`, prefix), sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StepUsage
	for rows.Next() {
		var (
			u               StepUsage
			stepID, agentID sql.NullString
			modal           string
		)
		if err := rows.Scan(&stepID, &agentID, &u.Provider, &u.Model, &modal,
			&u.InputTokens, &u.OutputTokens, &u.Images, &u.DurationSec,
			&u.USDCost, &u.Estimated, &u.Timestamp); err != nil {
			return nil, err
		}
		u.Modal = Modality(modal)
		if stepID.Valid {
			u.StepID, _ = uuid.Parse(stepID.String)
		}
		// agent_id is nullable and stays uuid.Nil when absent.
		if agentID.Valid {
			u.AgentID, _ = uuid.Parse(agentID.String)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
