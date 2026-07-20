package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// flow_store.go persists a Flow (agent map) as data so an editor can author it
// and RunTurnBySlug can execute it — turning a pipeline from Go code into an
// editable, versioned record.
//
// The map is an ordered list of agents: the first is the lead, the rest are
// followers (matching the runtime Flow's lead + concurrent followers). Owner is
// an opaque scope the embedding application controls (e.g. a tenant/studio id);
// "" is the global default. loom itself stays tenancy-agnostic — owner is just a
// string it filters on.

// FlowRecord is a persisted, versioned agent map.
type FlowRecord struct {
	ID        uuid.UUID
	Owner     string // opaque app-owned scope; "" = global
	Slug      string
	Version   int
	Category  string
	IsActive  bool
	Agents    []FlowAgentEntry // ordered; index 0 is the lead
	CreatedAt time.Time
}

// FlowAgentEntry is one agent's slot in a persisted flow.
type FlowAgentEntry struct {
	Position          int
	AgentSlug         string
	AgentVersion      int
	OutputKey         string
	Stream            bool
	GeneratorOverride string
	Params            map[string]any
	// RetryMode and MaxRetries mirror the runtime FlowAgent fields so a flow's
	// retry policy survives persistence. Before these were stored, a flow saved
	// with RetryKeepBest came back as RetryDiscard while the write reported
	// success. Their zero values (RetryDiscard, engine-default cap) are what
	// rows written before the columns existed resolve to.
	RetryMode  RetryMode
	MaxRetries int
}

// Flow builds the runtime Flow from the record: the first entry is the lead, the
// rest are followers, preserving order.
func (r *FlowRecord) Flow() Flow {
	f := Flow{Slug: r.Slug}
	for i, e := range r.Agents {
		fa := FlowAgent{
			AgentSlug:         e.AgentSlug,
			AgentVersion:      e.AgentVersion,
			Stream:            e.Stream,
			OutputKey:         e.OutputKey,
			GeneratorOverride: e.GeneratorOverride,
			Params:            e.Params,
			RetryMode:         e.RetryMode,
			MaxRetries:        e.MaxRetries,
		}
		if i == 0 {
			f.Lead = fa
		} else {
			f.Followers = append(f.Followers, fa)
		}
	}
	return f
}

// FlowRegistry resolves and manages persisted flows (agent maps). owner is an
// opaque scope ("" = global). On Get/Latest, version 0 resolves to the latest
// version; Create and Delete require an explicit version >= 1. Edits create a
// new version (like agents/prompts); Delete removes a specific version.
type FlowRegistry interface {
	Create(ctx context.Context, r *FlowRecord) error
	Get(ctx context.Context, owner, slug string, version int) (*FlowRecord, error)
	Latest(ctx context.Context, owner, slug string) (*FlowRecord, error)
	// List returns flows for an owner (optional category filter) WITHOUT their
	// agent entries — a lightweight index for an editor.
	List(ctx context.Context, owner, category string) ([]*FlowRecord, error)
	Delete(ctx context.Context, owner, slug string, version int) error
}

// flowService implements FlowRegistry.
type flowService struct{ e *Engine }

func (s *flowService) Create(ctx context.Context, r *FlowRecord) error {
	if len(r.Agents) == 0 {
		return fmt.Errorf("loom: flow %q: needs at least one agent (the lead)", r.Slug)
	}
	if r.Version < 1 {
		return fmt.Errorf("loom: flow %q: version must be >= 1 (0 is reserved for latest resolution)", r.Slug)
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	// The map is an ordered list with the lead first; derive positions from slice
	// order so callers don't have to set them.
	for i := range r.Agents {
		r.Agents[i].Position = i
	}
	return sqlInsertFlow(ctx, s.e.db, s.e.prefix, r)
}

func (s *flowService) Get(ctx context.Context, owner, slug string, version int) (*FlowRecord, error) {
	if version == 0 {
		return s.Latest(ctx, owner, slug)
	}
	return sqlQueryFlow(ctx, s.e.db, s.e.prefix, owner, slug, version)
}

func (s *flowService) Latest(ctx context.Context, owner, slug string) (*FlowRecord, error) {
	return sqlQueryFlowLatest(ctx, s.e.db, s.e.prefix, owner, slug)
}

func (s *flowService) List(ctx context.Context, owner, category string) ([]*FlowRecord, error) {
	return sqlListFlows(ctx, s.e.db, s.e.prefix, owner, category)
}

func (s *flowService) Delete(ctx context.Context, owner, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete flow %q: an explicit version >= 1 is required", slug)
	}
	return sqlDeleteFlow(ctx, s.e.db, s.e.prefix, owner, slug, version)
}

// RunTurnBySlug loads a persisted flow (owner/slug/version; version 0 = latest)
// and executes it as a turn, exactly like RunTurn on an in-code Flow. The
// caller's TurnRequest fields (Action, OnChunk, Inputs, Overrides, Params) are
// honoured; req.Flow is replaced by the loaded flow.
func (e *Engine) RunTurnBySlug(ctx context.Context, session *Session, owner, slug string, version int, req TurnRequest) (*Turn, error) {
	rec, err := e.flows.Get(ctx, owner, slug, version)
	if err != nil {
		return nil, fmt.Errorf("loom: load flow %q: %w", slug, err)
	}
	// A retired/disabled flow version must not execute. Re-activate by storing a
	// new version with IsActive set.
	if !rec.IsActive {
		return nil, fmt.Errorf("loom: flow %q v%d is not active", slug, rec.Version)
	}
	req.Flow = rec.Flow()
	// Resolve the flow's agents within the flow's own owner scope, so a
	// persisted flow can never reach an agent belonging to another tenant.
	// Taken from the loaded record rather than the caller's req so it cannot be
	// overridden from outside.
	req.Owner = rec.Owner
	return e.RunTurn(ctx, session, req)
}

// -----------------------------------------------------------------------
// SQL
// -----------------------------------------------------------------------

const flowColumns = `id, owner, slug, version, category, is_active, created_at`
const flowAgentColumns = `position, agent_slug, agent_version, output_key, stream, generator_override, params, retry_mode, max_retries`

func sqlInsertFlow(ctx context.Context, db *sql.DB, prefix string, r *FlowRecord) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sflows (id, owner, slug, version, category, is_active, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, prefix),
		r.ID, r.Owner, r.Slug, r.Version, r.Category, flowBool(r.IsActive), r.CreatedAt,
	); err != nil {
		return err
	}
	for _, e := range r.Agents {
		paramsJSON, _ := json.Marshal(e.Params)
		if len(paramsJSON) == 0 {
			paramsJSON = []byte("{}")
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO %sflow_agents
				(id, flow_id, position, agent_slug, agent_version, output_key, stream,
				 generator_override, params, retry_mode, max_retries)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, prefix),
			uuid.New(), r.ID, e.Position, e.AgentSlug, e.AgentVersion, e.OutputKey,
			flowBool(e.Stream), e.GeneratorOverride, paramsJSON, int(e.RetryMode), e.MaxRetries,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func sqlQueryFlow(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) (*FlowRecord, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM %sflows WHERE owner=$1 AND slug=$2 AND version=$3`, flowColumns, prefix),
		owner, slug, version)
	r, err := scanFlow(row)
	if err != nil {
		return nil, err
	}
	if err := loadFlowAgents(ctx, db, prefix, r); err != nil {
		return nil, err
	}
	return r, nil
}

func sqlQueryFlowLatest(ctx context.Context, db *sql.DB, prefix, owner, slug string) (*FlowRecord, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM %sflows WHERE owner=$1 AND slug=$2 ORDER BY version DESC LIMIT 1`, flowColumns, prefix),
		owner, slug)
	r, err := scanFlow(row)
	if err != nil {
		return nil, err
	}
	if err := loadFlowAgents(ctx, db, prefix, r); err != nil {
		return nil, err
	}
	return r, nil
}

func sqlListFlows(ctx context.Context, db *sql.DB, prefix, owner, category string) ([]*FlowRecord, error) {
	q := fmt.Sprintf(`SELECT %s FROM %sflows WHERE owner=$1`, flowColumns, prefix)
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
	var out []*FlowRecord
	for rows.Next() {
		r, err := scanFlow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func sqlDeleteFlow(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Remove child rows first — SQLite does not enforce the FK cascade.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM %sflow_agents WHERE flow_id IN
			(SELECT id FROM %sflows WHERE owner=$1 AND slug=$2 AND version=$3)`, prefix, prefix),
		owner, slug, version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM %sflows WHERE owner=$1 AND slug=$2 AND version=$3`, prefix),
		owner, slug, version); err != nil {
		return err
	}
	return tx.Commit()
}

type flowRow interface {
	Scan(dest ...any) error
}

func scanFlow(row flowRow) (*FlowRecord, error) {
	var (
		r        FlowRecord
		isActive int
	)
	err := row.Scan(&r.ID, &r.Owner, &r.Slug, &r.Version, &r.Category, &isActive, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.IsActive = isActive != 0
	return &r, nil
}

func loadFlowAgents(ctx context.Context, db *sql.DB, prefix string, r *FlowRecord) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s FROM %sflow_agents WHERE flow_id=$1 ORDER BY position`, flowAgentColumns, prefix),
		r.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			e          FlowAgentEntry
			stream     int
			paramsJSON []byte
			retryMode  int
		)
		if err := rows.Scan(&e.Position, &e.AgentSlug, &e.AgentVersion, &e.OutputKey,
			&stream, &e.GeneratorOverride, &paramsJSON, &retryMode, &e.MaxRetries); err != nil {
			return err
		}
		e.Stream = stream != 0
		e.RetryMode = RetryMode(retryMode)
		if len(paramsJSON) > 0 {
			_ = json.Unmarshal(paramsJSON, &e.Params)
		}
		r.Agents = append(r.Agents, e)
	}
	return rows.Err()
}

// flowBool stores a bool as INT so the column type is identical on Postgres and
// SQLite (avoids the bool-vs-int driver scan mismatch).
func flowBool(b bool) int {
	if b {
		return 1
	}
	return 0
}
