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

// FlowPlan is a flow's resolved wiring, for inspection and visualization — the
// same producer/consumer graph RunTurn executes and Flows().Validate checks,
// exposed as data. Unlike Validate it does NOT reject a bad flow: it represents
// the problems (unresolved inputs are edges of source "unresolved"; a cycle
// fills Cyclic) so a UI can render exactly why a flow is broken. A flow is valid
// iff no edge has source FlowEdgeUnresolved and Cyclic is empty.
type FlowPlan struct {
	// Nodes in execution order: the lead (Layer 0) first, then followers by
	// dependency layer.
	Nodes []FlowPlanNode
	// Edges describe every input variable a node consumes and where it resolves
	// from — the arrows a diagram draws.
	Edges []FlowPlanEdge
	// Layers is the number of follower dependency layers (0 with no followers).
	Layers int
	// Inputs the flow declares it receives from the caller (FlowRecord.Inputs).
	// The implicit "action" input is always available in addition to these.
	Inputs []string
	// Cyclic names followers that could not be placed in a layer because they
	// form a dependency cycle; empty for a valid flow.
	Cyclic []string
}

// FlowPlanNode is one agent in a plan, with what it produces and consumes.
type FlowPlanNode struct {
	AgentSlug    string
	AgentVersion int
	IsLead       bool
	// Layer is 0 for the lead and 1..Layers for a follower (its execution
	// layer). A cyclic follower is placed after the last real layer.
	Layer int
	// OutputKey is the name this agent's output is exposed under ("" = it
	// produces no consumable output).
	OutputKey string
	// Consumes is the input variable names this agent's user template declares
	// (its Prompt.Variables).
	Consumes []string
}

// FlowEdgeSource says where a consumed variable comes from.
type FlowEdgeSource string

const (
	// FlowEdgeAgent — produced by another agent (the edge's From).
	FlowEdgeAgent FlowEdgeSource = "agent"
	// FlowEdgeInput — supplied by the caller (a flow input or the implicit
	// "action").
	FlowEdgeInput FlowEdgeSource = "input"
	// FlowEdgeUnresolved — nothing produces it and it is not a declared input: a
	// broken wire (the lead consuming an agent output also lands here, since the
	// lead runs first).
	FlowEdgeUnresolved FlowEdgeSource = "unresolved"
)

// FlowPlanEdge is one consumed variable flowing into a node.
type FlowPlanEdge struct {
	// To is the consuming agent's slug; Var is the variable that flows.
	To  string
	Var string
	// Source classifies the origin; From is the producing agent's slug only when
	// Source == FlowEdgeAgent.
	Source FlowEdgeSource
	From   string
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
	// Plan returns the flow's resolved wiring — nodes, edges, execution layers,
	// and any dependency cycle — for inspection and visualization. It is what
	// Validate checks and RunTurn executes, exposed as data, and does not error
	// on a bad flow (it represents the problems). See FlowPlan.
	Plan(ctx context.Context, r *FlowRecord) (*FlowPlan, error)
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

	// Names a follower produces — a valid source for another follower, since
	// layered execution runs a producer before its consumer (acyclicity is
	// checked below). The lead cannot consume a follower (it runs first).
	followerProduces := map[string]bool{}
	for _, e := range r.Agents[1:] {
		if e.OutputKey != "" {
			followerProduces[e.OutputKey] = true
		}
	}

	followers := r.Agents[1:]
	followerConsumed := make([][]string, len(followers))
	followerKeys := make([]string, len(followers))
	for i, e := range followers {
		followerKeys[i] = e.OutputKey
	}

	for i, e := range r.Agents {
		isLead := i == 0
		consumed, err := s.agentConsumes(ctx, r.Owner, e)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !isLead {
			followerConsumed[i-1] = consumed
		}
		for _, name := range consumed {
			switch {
			case external[name]:
				// Supplied at RunTurn time.
			case !isLead && name == leadKey:
				// A follower may read the lead's output.
			case !isLead && followerProduces[name]:
				// A follower consuming a sibling — valid; ordering handled by
				// layered execution, cycles caught below.
			case isLead && (name == leadKey || followerProduces[name]):
				// The lead runs first: it cannot consume its own output or a
				// follower's.
				errs = append(errs, fmt.Errorf(
					"lead agent %q consumes %q, but the lead runs first and can read only declared flow inputs",
					e.AgentSlug, name))
			default:
				errs = append(errs, fmt.Errorf(
					"agent %q consumes %q, which no agent produces and the flow does not declare as an input",
					e.AgentSlug, name))
			}
		}
	}

	// Followers must be orderable — a dependency cycle cannot be scheduled.
	if _, cyclic := topoLayers(followerKeys, followerConsumed); len(cyclic) > 0 {
		names := make([]string, len(cyclic))
		for i, idx := range cyclic {
			names[i] = followers[idx].AgentSlug
		}
		errs = append(errs, fmt.Errorf("followers form a dependency cycle: %v", names))
	}

	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrFlowInvalid, errors.Join(errs...))
	}
	return nil
}

func (s *flowService) Plan(ctx context.Context, r *FlowRecord) (*FlowPlan, error) {
	if len(r.Agents) == 0 {
		return nil, fmt.Errorf("%w: needs at least one agent (the lead)", ErrFlowInvalid)
	}

	// Resolve every agent's consumed variables best-effort: a missing agent
	// yields no edges but still appears as a node, so Plan renders a partially
	// broken flow rather than failing (Validate is the authoritative check).
	consumes := make([][]string, len(r.Agents))
	for i, e := range r.Agents {
		consumes[i] = s.e.userTemplateVars(ctx, r.Owner, e.AgentSlug, e.AgentVersion)
	}

	lead := r.Agents[0]
	leadKey := leadOutputKey(lead)
	followers := r.Agents[1:]

	// producer[outputKey] = producing agent slug (lead + followers).
	producer := map[string]string{}
	if leadKey != "" {
		producer[leadKey] = lead.AgentSlug
	}
	for _, f := range followers {
		if f.OutputKey != "" {
			producer[f.OutputKey] = f.AgentSlug
		}
	}
	external := map[string]bool{"action": true}
	for _, in := range r.Inputs {
		external[in] = true
	}

	// Layer each follower.
	fKeys := make([]string, len(followers))
	fConsumes := make([][]string, len(followers))
	for i, f := range followers {
		fKeys[i] = f.OutputKey
		fConsumes[i] = consumes[i+1]
	}
	layers, cyclic := topoLayers(fKeys, fConsumes)
	layerOf := make([]int, len(followers))
	for _, li := range cyclic {
		layerOf[li] = len(layers) + 1 // cyclic followers run after the last real layer
	}
	for li, layer := range layers {
		for _, idx := range layer {
			layerOf[idx] = li + 1 // followers occupy layers 1..N; the lead is 0
		}
	}

	plan := &FlowPlan{Layers: len(layers), Inputs: append([]string(nil), r.Inputs...)}
	plan.Nodes = append(plan.Nodes, FlowPlanNode{
		AgentSlug: lead.AgentSlug, AgentVersion: lead.AgentVersion,
		IsLead: true, Layer: 0, OutputKey: leadKey, Consumes: consumes[0],
	})
	for i, f := range followers {
		plan.Nodes = append(plan.Nodes, FlowPlanNode{
			AgentSlug: f.AgentSlug, AgentVersion: f.AgentVersion,
			Layer: layerOf[i], OutputKey: f.OutputKey, Consumes: consumes[i+1],
		})
	}
	for _, idx := range cyclic {
		plan.Cyclic = append(plan.Cyclic, followers[idx].AgentSlug)
	}

	// One edge per consumed variable, classified by where it resolves from. The
	// classification matches Validate exactly, so an edge is FlowEdgeUnresolved
	// precisely when Validate would report that consumption as a problem.
	for i, e := range r.Agents {
		isLead := i == 0
		for _, name := range consumes[i] {
			edge := FlowPlanEdge{To: e.AgentSlug, Var: name}
			from, ok := producer[name]
			switch {
			case external[name]:
				edge.Source = FlowEdgeInput
			case ok && !isLead && from != e.AgentSlug:
				// A follower reading the lead's or a sibling's output. Ordering
				// is handled by layering; a cycle shows up in plan.Cyclic.
				edge.Source = FlowEdgeAgent
				edge.From = from
			default:
				// Nothing produces it, a self-reference, or the lead consuming an
				// agent output (the lead runs first) — all broken wires.
				edge.Source = FlowEdgeUnresolved
			}
			plan.Edges = append(plan.Edges, edge)
		}
	}
	return plan, nil
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
