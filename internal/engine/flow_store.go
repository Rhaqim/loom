package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	ID       uuid.UUID
	Owner    string // opaque app-owned scope; "" = global
	Slug     string
	Version  int
	Category string
	IsActive bool
	// Inputs names the variables this flow expects to be supplied at RunTurn
	// time (via TurnRequest.Inputs), as opposed to being produced by an agent
	// in the flow. Validate treats these as external producers: an agent
	// consuming one of these needs no in-flow producer for it. Optional; a flow
	// that declares nothing is unaffected.
	Inputs    []string
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
	// Latest resolves the highest version of slug within owner REGARDLESS of
	// IsActive — the authoring view, so an editor can see a draft it just
	// saved. Use LatestActive for the serving view.
	Latest(ctx context.Context, owner, slug string) (*FlowRecord, error)
	// LatestActive resolves the highest version of slug within owner that has
	// IsActive set — the serving view, and what version 0 resolves to on Get
	// and RunTurnBySlug.
	//
	// This is what makes a draft safe: saving v5 with IsActive false leaves v4
	// serving traffic instead of taking the slug down. Returns *NotFoundError
	// if no active version exists. If several versions are active (the default
	// before SetActive is used — Create does not deactivate siblings), the
	// highest wins, which is the historical behaviour.
	LatestActive(ctx context.Context, owner, slug string) (*FlowRecord, error)
	// SetActive makes one version the active one and deactivates every other
	// version of the same owner+slug, in a single transaction.
	//
	// This turns publish and rollback into a pointer move rather than "write a
	// newer version", which is the only thing that makes rolling BACK to an
	// older version possible at all. Returns *NotFoundError if the version does
	// not exist, leaving the current pointer untouched.
	SetActive(ctx context.Context, owner, slug string, version int) error
	// List returns flows for an owner (optional category filter) WITHOUT their
	// agent entries — a lightweight index for an editor.
	List(ctx context.Context, owner, category string) ([]*FlowRecord, error)
	Delete(ctx context.Context, owner, slug string, version int) error
	// Validate checks a flow's variable wiring against the current registry
	// state WITHOUT persisting anything, so an editor can call it before Create.
	//
	// It resolves each agent's declared input variables (its user template's
	// Prompt.Variables) and confirms every one is satisfiable, then checks that
	// producer names are unique. Returns nil when the wiring is sound, or an
	// ErrFlowInvalid joining every problem found (dangling variable, duplicate
	// producer, unknown referenced agent). Create runs the self-contained subset
	// of these checks; Validate runs the full resolving check.
	//
	// A flow whose agents declare no input variables passes trivially, so this
	// is a no-op for flows that do not opt into wiring.
	Validate(ctx context.Context, r *FlowRecord) error
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
	// Self-contained wiring checks (no agent/prompt resolution, so Create does
	// not depend on the referenced agents existing yet). The full consumed-vs-
	// produced check that resolves prompts is Flows().Validate.
	if errs := flowProducerConflicts(r); len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrFlowInvalid, errors.Join(errs...))
	}
	if err := sqlInsertFlow(ctx, s.e.db, s.e.prefix, r); err != nil {
		return err
	}
	// An active new version can become the latest-active, so evict the pointer.
	// A draft (IsActive false) does not move it, but evicting anyway is cheap
	// and keeps the rule "any write to this slug evicts" simple.
	cacheDelete(ctx, s.e.cache, s.e.cacheKeyOwnedLatest("flow-active", r.Owner, r.Slug))
	return nil
}

func (s *flowService) Get(ctx context.Context, owner, slug string, version int) (*FlowRecord, error) {
	if version == 0 {
		// Version 0 means "whatever serves traffic", so it resolves the latest
		// ACTIVE version. Resolving the latest version and then refusing it for
		// being inactive is never useful: it lets an unfinished draft take a
		// slug down instead of sitting harmlessly beside the live version. Use
		// Latest explicitly for the authoring view that includes drafts.
		return s.LatestActive(ctx, owner, slug)
	}
	return sqlQueryFlow(ctx, s.e.db, s.e.prefix, owner, slug, version)
}

func (s *flowService) Latest(ctx context.Context, owner, slug string) (*FlowRecord, error) {
	return sqlQueryFlowLatest(ctx, s.e.db, s.e.prefix, owner, slug)
}

func (s *flowService) LatestActive(ctx context.Context, owner, slug string) (*FlowRecord, error) {
	// Flows are otherwise uncached; the serving resolver (also what version 0
	// and RunTurnBySlug use) is the hot read, so it caches the active pointer
	// under the short latest TTL, evicted on SetActive / Create / Delete.
	key := s.e.cacheKeyOwnedLatest("flow-active", owner, slug)
	return cachedLatest(ctx, s.e, key, func() (*FlowRecord, error) {
		return sqlQueryFlowLatestActive(ctx, s.e.db, s.e.prefix, owner, slug)
	})
}

func (s *flowService) SetActive(ctx context.Context, owner, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: set active flow %q: an explicit version >= 1 is required", slug)
	}
	if err := sqlSetFlowActive(ctx, s.e.db, s.e.prefix, owner, slug, version); err != nil {
		return err
	}
	// SetActive is the publish/rollback pointer move, so the cached active
	// resolution is now stale by definition — evict it.
	cacheDelete(ctx, s.e.cache, s.e.cacheKeyOwnedLatest("flow-active", owner, slug))
	return nil
}

func (s *flowService) List(ctx context.Context, owner, category string) ([]*FlowRecord, error) {
	return sqlListFlows(ctx, s.e.db, s.e.prefix, owner, category)
}

func (s *flowService) Delete(ctx context.Context, owner, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete flow %q: an explicit version >= 1 is required", slug)
	}
	if err := sqlDeleteFlow(ctx, s.e.db, s.e.prefix, owner, slug, version); err != nil {
		return err
	}
	// Deleting the active version moves the pointer, so evict it.
	cacheDelete(ctx, s.e.cache, s.e.cacheKeyOwnedLatest("flow-active", owner, slug))
	return nil
}

// leadOutputKey returns a FlowAgentEntry's effective output key: the lead
// defaults to "Lead" (matching RunTurn), so the two agree on the name the lead's
// output is exposed under.
func leadOutputKey(e FlowAgentEntry) string {
	if e.OutputKey == "" {
		return "Lead"
	}
	return e.OutputKey
}

// flowProducerConflicts returns the wiring problems detectable WITHOUT resolving
// any agent or prompt: two agents producing the same output name, or an output
// name shadowing a declared external input. Both are ambiguities — the shared
// inputs map has one slot per name — so they are refused rather than silently
// last-writer-wins.
func flowProducerConflicts(r *FlowRecord) []error {
	var errs []error
	seen := map[string]int{} // output name -> count
	for i, e := range r.Agents {
		key := e.OutputKey
		if i == 0 {
			key = leadOutputKey(e)
		}
		if key == "" {
			continue // a follower with no output key produces nothing consumable
		}
		seen[key]++
		if seen[key] == 2 {
			errs = append(errs, fmt.Errorf("output key %q is produced by more than one agent", key))
		}
	}
	external := map[string]bool{}
	for _, in := range r.Inputs {
		external[in] = true
	}
	for name := range seen {
		if external[name] {
			errs = append(errs, fmt.Errorf("output key %q collides with a declared flow input", name))
		}
	}
	return errs
}

func (s *flowService) Validate(ctx context.Context, r *FlowRecord) error {
	if len(r.Agents) == 0 {
		return fmt.Errorf("%w: needs at least one agent (the lead)", ErrFlowInvalid)
	}
	errs := flowProducerConflicts(r)

	// External producers every agent can see: the flow's declared inputs plus
	// "action", which RunTurn injects into every agent's inputs.
	external := map[string]bool{"action": true}
	for _, in := range r.Inputs {
		external[in] = true
	}
	leadKey := leadOutputKey(r.Agents[0])

	// The set of names produced only by a follower — used to give a precise
	// message when a consumer depends on a sibling, which layered execution does
	// not run yet (see 28-Variable-Producer-Consumer.md, phase 2).
	followerProduces := map[string]bool{}
	for _, e := range r.Agents[1:] {
		if e.OutputKey != "" {
			followerProduces[e.OutputKey] = true
		}
	}

	for i, e := range r.Agents {
		isLead := i == 0
		consumed, err := s.agentConsumes(ctx, r.Owner, e)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, name := range consumed {
			switch {
			case external[name]:
				// Supplied at RunTurn time.
			case !isLead && name == leadKey:
				// A follower may read the lead's output.
			case followerProduces[name]:
				// Produced, but only by a sibling follower. The lead cannot
				// consume a follower at all; a follower consuming another
				// follower needs layered execution, which is not enabled yet.
				errs = append(errs, fmt.Errorf(
					"agent %q consumes %q, produced only by a follower — cross-follower wiring is not executed yet (phase 2)",
					e.AgentSlug, name))
			default:
				errs = append(errs, fmt.Errorf(
					"agent %q consumes %q, which no agent produces and the flow does not declare as an input",
					e.AgentSlug, name))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrFlowInvalid, errors.Join(errs...))
	}
	return nil
}

// agentConsumes resolves the input-variable names an agent's user template
// declares (its user-template Prompt.Variables), under the flow's owner scope.
// An agent with no user template consumes nothing declared.
func (s *flowService) agentConsumes(ctx context.Context, owner string, e FlowAgentEntry) ([]string, error) {
	agent, err := s.e.agents.Get(ctx, owner, e.AgentSlug, e.AgentVersion)
	if err != nil {
		return nil, fmt.Errorf("flow references agent %q@v%d: %w", e.AgentSlug, e.AgentVersion, err)
	}
	if agent.UserTemplateID == uuid.Nil {
		return nil, nil
	}
	ut, err := s.e.prompts.GetByID(ctx, agent.UserTemplateID)
	if err != nil {
		return nil, fmt.Errorf("agent %q user template: %w", e.AgentSlug, err)
	}
	return ut.Variables, nil
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
	// A retired/disabled flow version must not execute. With version 0 this is
	// unreachable — Get already resolved the latest ACTIVE version — so it only
	// fires when a caller pins an explicitly inactive version, which is a
	// caller mistake rather than a draft taking the slug down. Publish with
	// Flows().SetActive.
	if !rec.IsActive {
		return nil, fmt.Errorf("loom: flow %q v%d is not active (publish it with Flows().SetActive)", slug, rec.Version)
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

const flowColumns = `id, owner, slug, version, category, is_active, inputs, created_at`
const flowAgentColumns = `position, agent_slug, agent_version, output_key, stream, generator_override, params, retry_mode, max_retries`

func sqlInsertFlow(ctx context.Context, db *sql.DB, prefix string, r *FlowRecord) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	inputsJSON, _ := json.Marshal(r.Inputs)
	if len(inputsJSON) == 0 || string(inputsJSON) == "null" {
		inputsJSON = []byte("[]")
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sflows (id, owner, slug, version, category, is_active, inputs, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, prefix),
		r.ID, r.Owner, r.Slug, r.Version, r.Category, flowBool(r.IsActive), inputsJSON, r.CreatedAt,
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

// sqlQueryFlowLatestActive resolves the highest ACTIVE version. is_active is
// stored as an INT (SQLite has no bool), so compare against 1 rather than using
// a boolean predicate, which Postgres would accept but SQLite would not.
func sqlQueryFlowLatestActive(ctx context.Context, db *sql.DB, prefix, owner, slug string) (*FlowRecord, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM %sflows WHERE owner=$1 AND slug=$2 AND is_active=1
		ORDER BY version DESC LIMIT 1`, flowColumns, prefix),
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

// sqlSetFlowActive points owner+slug at one version: it activates that version
// and deactivates every sibling, in one transaction so no window exists where
// the slug has zero or two active versions.
func sqlSetFlowActive(ctx context.Context, db *sql.DB, prefix, owner, slug string, version int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Activate the target first, so a version that does not exist is caught
	// BEFORE the siblings are deactivated — otherwise a typo would leave the
	// slug with nothing serving.
	res, err := tx.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %sflows SET is_active=1 WHERE owner=$1 AND slug=$2 AND version=$3`, prefix),
		owner, slug, version)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return &NotFoundError{Kind: "flow", Key: fmt.Sprintf("%s@v%d", slug, version)}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %sflows SET is_active=0 WHERE owner=$1 AND slug=$2 AND version<>$3`, prefix),
		owner, slug, version); err != nil {
		return err
	}
	return tx.Commit()
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
		r          FlowRecord
		isActive   int
		inputsJSON []byte
	)
	err := row.Scan(&r.ID, &r.Owner, &r.Slug, &r.Version, &r.Category, &isActive, &inputsJSON, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.IsActive = isActive != 0
	if len(inputsJSON) > 0 {
		_ = json.Unmarshal(inputsJSON, &r.Inputs)
	}
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
