package loom

// persist.go contains all SQL persistence functions used by engine.go.
// All functions accept a prefix string to namespace table names (e.g. "loom_").

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
// Agent persistence
// -----------------------------------------------------------------------

const agentColumns = `id, slug, version, category, modality, generator_slug,
	system_prompt_id, user_template_id, response_format_id, response_format, params,
	fallback_agent_id, created_at`

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
			 fallback_agent_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		prefix),
		a.ID, a.Slug, a.Version, a.Category, string(a.Modal),
		a.GeneratorSlug, nullUUID(a.SystemPromptID), nullUUID(a.UserTemplateID),
		rfID, rfJSON, paramsJSON, fallbackID, a.CreatedAt,
	)
	return err
}

func sqlQueryAgent(ctx context.Context, db *sql.DB, prefix, slug string, version int) (*Agent, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %sagents WHERE slug=$1 AND version=$2`, agentColumns, prefix), slug, version)
	return scanAgent(row)
}

func sqlQueryAgentLatest(ctx context.Context, db *sql.DB, prefix, slug string) (*Agent, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %sagents WHERE slug=$1 ORDER BY version DESC LIMIT 1`, agentColumns, prefix), slug)
	return scanAgent(row)
}

func sqlListAgents(ctx context.Context, db *sql.DB, prefix, category string) ([]*Agent, error) {
	q := fmt.Sprintf(`SELECT %s FROM %sagents`, agentColumns, prefix)
	args := []any{}
	if category != "" {
		q += " WHERE category=$1"
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
		&fallbackID, &a.CreatedAt,
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

// -----------------------------------------------------------------------
// Response-format persistence (reusable, versioned records)
// -----------------------------------------------------------------------

const responseFormatColumns = "id, slug, version, schema, strict, created_at"

func sqlInsertResponseFormat(ctx context.Context, db *sql.DB, prefix string, rf *ResponseFormatRecord) error {
	schemaJSON, _ := json.Marshal(rf.Schema)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sresponse_formats (id, slug, version, schema, strict, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, prefix),
		rf.ID, rf.Slug, rf.Version, schemaJSON, rf.StrictMode, rf.CreatedAt,
	)
	return err
}

func sqlQueryResponseFormat(ctx context.Context, db *sql.DB, prefix, slug string, version int) (*ResponseFormatRecord, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %sresponse_formats WHERE slug=$1 AND version=$2`, responseFormatColumns, prefix), slug, version)
	return scanResponseFormat(row)
}

func sqlQueryResponseFormatLatest(ctx context.Context, db *sql.DB, prefix, slug string) (*ResponseFormatRecord, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %sresponse_formats WHERE slug=$1 ORDER BY version DESC LIMIT 1`, responseFormatColumns, prefix), slug)
	return scanResponseFormat(row)
}

func sqlQueryResponseFormatByID(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*ResponseFormatRecord, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %sresponse_formats WHERE id=$1`, responseFormatColumns, prefix), id)
	return scanResponseFormat(row)
}

func scanResponseFormat(row promptRow) (*ResponseFormatRecord, error) {
	var (
		r          ResponseFormatRecord
		schemaJSON []byte
	)
	err := row.Scan(&r.ID, &r.Slug, &r.Version, &schemaJSON, &r.StrictMode, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(schemaJSON, &r.Schema)
	return &r, nil
}

// -----------------------------------------------------------------------
// Prompt persistence
// -----------------------------------------------------------------------

const promptColumns = "id, slug, version, kind, category, body, variables, metadata, created_at, notes"

func sqlInsertPrompt(ctx context.Context, db *sql.DB, prefix string, p *Prompt) error {
	varsJSON, _ := json.Marshal(p.Variables)
	metaJSON, _ := json.Marshal(p.Metadata)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sprompts
			(id, slug, version, kind, category, body, variables, metadata, created_at, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, prefix),
		p.ID, p.Slug, p.Version, string(p.Kind), p.Category,
		p.Body, varsJSON, metaJSON, p.CreatedAt, p.Notes,
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
		&p.Body, &varsJSON, &metaJSON, &p.CreatedAt, &p.Notes)
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

// -----------------------------------------------------------------------
// Session persistence
// -----------------------------------------------------------------------

func sqlInsertSession(ctx context.Context, db *sql.DB, prefix string, s *Session) error {
	stateJSON, _ := json.Marshal(s.State)
	metaJSON, _ := json.Marshal(s.Metadata)
	tagsJSON, _ := json.Marshal(s.Tags)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %ssessions
			(id, platform_id, parent_session_id, branch_point,
			 state, metadata, tags, pinned, deleted_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, prefix),
		s.ID, s.PlatformID, nullableUUID(s.ParentSessionID), s.BranchPoint,
		stateJSON, metaJSON, tagsJSON, s.Pinned, s.DeletedAt,
		s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func sqlQuerySession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Session, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT id, platform_id, parent_session_id, branch_point,
		       state, metadata, tags, pinned, deleted_at, created_at, updated_at
		FROM %ssessions WHERE id=$1`, prefix), id)
	return scanSession(row)
}

func sqlUpdateSession(ctx context.Context, db *sql.DB, prefix string, s *Session) error {
	stateJSON, _ := json.Marshal(s.State)
	metaJSON, _ := json.Marshal(s.Metadata)
	tagsJSON, _ := json.Marshal(s.Tags)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %ssessions
		SET state=$2, metadata=$3, tags=$4, pinned=$5, updated_at=$6
		WHERE id=$1`, prefix),
		s.ID, stateJSON, metaJSON, tagsJSON, s.Pinned, s.UpdatedAt,
	)
	return err
}

func sqlListSessions(ctx context.Context, db *sql.DB, prefix, platformID string, limit, offset int) ([]*Session, error) {
	q := fmt.Sprintf(`
		SELECT id, platform_id, parent_session_id, branch_point,
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

func sqlBuildBranchTree(ctx context.Context, db *sql.DB, prefix string, rootID uuid.UUID) (*BranchNode, error) {
	root, err := sqlQuerySession(ctx, db, prefix, rootID)
	if err != nil {
		return nil, err
	}
	node := &BranchNode{Session: root}
	if err := buildChildren(ctx, db, prefix, node); err != nil {
		return nil, err
	}
	return node, nil
}

func buildChildren(ctx context.Context, db *sql.DB, prefix string, node *BranchNode) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, platform_id, parent_session_id, branch_point,
		       state, metadata, tags, pinned, deleted_at, created_at, updated_at
		FROM %ssessions WHERE parent_session_id=$1 AND deleted_at IS NULL
		ORDER BY created_at`, prefix), node.Session.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		child, err := scanSession(rows)
		if err != nil {
			return err
		}
		childNode := &BranchNode{Session: child}
		if err := buildChildren(ctx, db, prefix, childNode); err != nil {
			return err
		}
		node.Children = append(node.Children, childNode)
	}
	return rows.Err()
}

func sqlSetPinned(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, pinned bool) error {
	_, err := db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %ssessions SET pinned=$2, updated_at=$3 WHERE id=$1`, prefix),
		id, pinned, time.Now())
	return err
}

func sqlSoftDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	now := time.Now()
	_, err := db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %ssessions SET deleted_at=$2, updated_at=$3 WHERE id=$1`, prefix),
		id, now, now)
	return err
}

func sqlHardDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	_, err := db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %ssessions WHERE id=$1`, prefix), id)
	return err
}

type sessionRow interface {
	Scan(dest ...any) error
}

func scanSession(row sessionRow) (*Session, error) {
	var (
		s                             Session
		parentSessionID               sql.NullString
		branchPoint                   sql.NullInt64
		stateJSON, metaJSON, tagsJSON []byte
		deletedAt                     sql.NullTime
	)
	err := row.Scan(
		&s.ID, &s.PlatformID, &parentSessionID, &branchPoint,
		&stateJSON, &metaJSON, &tagsJSON, &s.Pinned, &deletedAt,
		&s.CreatedAt, &s.UpdatedAt,
	)
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

// -----------------------------------------------------------------------
// Step persistence
// -----------------------------------------------------------------------

// execer is satisfied by both *sql.DB and *sql.Tx, letting the row-insert
// helpers run either standalone or inside a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func sqlInsertStep(ctx context.Context, db *sql.DB, prefix string, step *Step) error {
	reqJSON, _ := json.Marshal(step.Request)
	diagJSON, _ := json.Marshal(step.Diagnostics)
	annJSON, _ := json.Marshal(step.Annotations)

	// All three writes (action, result, step) commit atomically so a failure on
	// the final steps INSERT cannot orphan the result/action rows.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var actionID *string
	if step.Action != nil {
		s := step.Action.ID.String()
		actionID = &s
		if err := sqlInsertAction(ctx, tx, prefix, step.SessionID, step.Index, step.Action); err != nil {
			return fmt.Errorf("persist action: %w", err)
		}
	}
	if err := sqlInsertResult(ctx, tx, prefix, step); err != nil {
		return fmt.Errorf("persist result: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %ssteps
			(id, session_id, step_index, agent_id, request, result_id,
			 action_id, annotations, diagnostics, duration_ms, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, prefix),
		step.ID, step.SessionID, step.Index, step.AgentID,
		reqJSON, step.Result.ResultID(),
		actionID, annJSON, diagJSON, step.DurationMs, step.CreatedAt,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func sqlInsertResult(ctx context.Context, db execer, prefix string, step *Step) error {
	payload, _ := marshalResultForStorage(step.Result)
	var taskID *string
	if th := step.Result.TaskHandle(); th != nil {
		s := th.ID.String()
		taskID = &s
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sresults (id, modality, status, payload, task_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, prefix),
		step.Result.ResultID(), string(step.Result.Modality()),
		string(step.Result.Status()), payload, taskID,
		time.Now(), time.Now(),
	)
	return err
}

func sqlInsertAction(ctx context.Context, db execer, prefix string, sessionID uuid.UUID, stepIndex int, action *Action) error {
	payloadJSON, _ := json.Marshal(action.Payload)
	metaJSON, _ := json.Marshal(action.Metadata)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sactions (id, session_id, step_index, kind, payload, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, prefix),
		action.ID, sessionID, stepIndex, string(action.Kind),
		payloadJSON, metaJSON, time.Now(),
	)
	return err
}

func sqlQuerySteps(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID) ([]Step, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT s.id, s.session_id, s.step_index, s.agent_id,
		       s.request, s.diagnostics, s.duration_ms, s.created_at,
		       r.modality, r.status, r.payload
		FROM %ssteps s
		JOIN %sresults r ON r.id = s.result_id
		WHERE s.session_id=$1
		ORDER BY s.step_index`, prefix, prefix), sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []Step
	for rows.Next() {
		var (
			step              Step
			reqJSON, diagJSON []byte
			modal, status     string
			payload           []byte
		)
		if err := rows.Scan(
			&step.ID, &step.SessionID, &step.Index, &step.AgentID,
			&reqJSON, &diagJSON, &step.DurationMs, &step.CreatedAt,
			&modal, &status, &payload,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(reqJSON, &step.Request)
		_ = json.Unmarshal(diagJSON, &step.Diagnostics)
		step.Result = unmarshalResult(Modality(modal), ResultStatus(status), payload)
		steps = append(steps, step)
	}
	return steps, rows.Err()
}

// -----------------------------------------------------------------------
// Task persistence (async provider jobs)
// -----------------------------------------------------------------------

func sqlListPendingTasks(ctx context.Context, db *sql.DB, prefix string) ([]pendingTask, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, provider, handle FROM %stasks WHERE status='pending'`, prefix))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []pendingTask
	for rows.Next() {
		var t pendingTask
		if err := rows.Scan(&t.ID, &t.Provider, &t.Handle); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func sqlIncrementTaskAttempts(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, lastErr string) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %stasks SET attempts=attempts+1, last_error=$2, updated_at=$3 WHERE id=$1`,
		prefix), id, lastErr, time.Now())
	return err
}

func sqlMarkTaskReady(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, _ Result) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %stasks SET status='ready', updated_at=$2 WHERE id=$1`, prefix),
		id, time.Now())
	return err
}

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

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id.String()
}

func nullableUUID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

func nullableUUID2(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id.String()
}

func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

func nullInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

// marshalResultForStorage serialises a Result to JSON for loom_results.payload.
func marshalResultForStorage(r Result) ([]byte, error) {
	switch v := r.(type) {
	case *TextResult:
		return json.Marshal(map[string]any{
			"content":       v.Content,
			"finish_reason": v.FinishReason,
			"input_tokens":  v.InputTokens,
			"output_tokens": v.OutputTokens,
		})
	case *ImageResult:
		return json.Marshal(map[string]any{
			"url":    v.URL,
			"width":  v.Width,
			"height": v.Height,
			"prompt": v.Prompt,
		})
	case *VideoResult:
		return json.Marshal(map[string]any{
			"url":           v.URL,
			"duration_sec":  v.DurationSec,
			"width":         v.Width,
			"height":        v.Height,
			"preview_image": v.PreviewImage,
		})
	case *WorldResult:
		return json.Marshal(map[string]any{"deltas": v.Deltas})
	case *StructuredResult:
		return json.Marshal(map[string]any{
			"data":          v.Data,
			"input_tokens":  v.InputTokens,
			"output_tokens": v.OutputTokens,
		})
	default:
		return json.Marshal(map[string]any{"modality": string(r.Modality())})
	}
}

// unmarshalResult deserialises a stored payload back to a Result.
func unmarshalResult(modal Modality, status ResultStatus, payload []byte) Result {
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	switch modal {
	case ModalityText:
		r := &TextResult{}
		r.modal = ModalityText
		r.status = status
		r.meta = map[string]any{}
		if c, ok := m["content"].(string); ok {
			r.Content = c
		}
		if fr, ok := m["finish_reason"].(string); ok {
			r.FinishReason = fr
		}
		return r
	case ModalityImage:
		r := &ImageResult{}
		r.modal = ModalityImage
		r.status = status
		r.meta = map[string]any{}
		if u, ok := m["url"].(string); ok {
			r.URL = u
		}
		return r
	case ModalityVideo:
		r := &VideoResult{}
		r.modal = ModalityVideo
		r.status = status
		r.meta = map[string]any{}
		if u, ok := m["url"].(string); ok {
			r.URL = u
		}
		return r
	case ModalityWorld:
		r := &WorldResult{}
		r.modal = ModalityWorld
		r.status = status
		r.meta = map[string]any{}
		return r
	default:
		r := &StructuredResult{}
		r.modal = modal
		r.status = status
		r.meta = map[string]any{}
		r.Data = m
		return r
	}
}
