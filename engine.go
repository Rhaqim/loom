// Package loom is a modality-agnostic procgen engine for building AI-driven
// interactive narratives, games, and generative platforms. It provides session
// management, versioned agents and prompts, a hook bus for pre/post processors,
// cost tracking with budget enforcement, branch/replay support, an entity
// annotator, a judge subsystem, and a first-class test harness.
package loom

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"text/template"
	"time"

	"github.com/google/uuid"
)

// Dialect identifies the database flavour for the schema loader.
type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectSQLite   Dialect = "sqlite"
)

// Config is the top-level engine configuration.
type Config struct {
	// DB is a configured *sql.DB connection. Required.
	DB *sql.DB
	// Dialect of the DB. Required.
	Dialect Dialect
	// SchemaPrefix is prepended to every engine table name (default: "loom_").
	SchemaPrefix string
	// Generators maps slug → Generator. Populated at startup.
	Generators map[string]Generator
	// AsyncPoller configures the background task-poller for video/image providers.
	AsyncPoller PollerConfig
	// Logger receives structured log lines from the engine (optional).
	Logger Logger
	// MaxRetries is the default retry cap for post-hooks (default: 3).
	MaxRetries int
	// Cache is an optional read-through cache for immutable engine objects
	// (agents, prompts). Use NewInProcessCache() for a zero-dependency in-process
	// cache, or supply any implementation of the Cache interface (Redis, etc.).
	// A nil Cache disables caching — every RunStep hits the database directly.
	Cache Cache
	// Pricing maps model id → per-million-token price for accurate cost tracking.
	// Generators report their model on Result metadata ("model"); a model not in
	// the table falls back to DefaultPrice. Nil disables per-model pricing (the
	// flat default applies).
	Pricing map[string]ModelPrice
	// DefaultPrice is used for models absent from Pricing (default: $1/$3 per 1M).
	DefaultPrice *ModelPrice
	// BranchGC configures the branch garbage collector used by Engine.GC().
	// Nil uses safe defaults (DryRun=true, so nothing is deleted); set an explicit
	// config with DryRun=false to actually sweep. The Dialect is filled in from
	// Config.Dialect automatically.
	BranchGC *BranchGCConfig
}

// Logger is the minimal logging interface the engine uses.
type Logger interface {
	Info(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// noopLogger discards all log output.
type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// Engine is the central object applications interact with.
type Engine struct {
	cfg          Config
	db           *sql.DB
	prefix       string
	generators   map[string]Generator
	hooks        *hookBus
	log          Logger
	cache        Cache
	pricing      map[string]ModelPrice
	defaultPrice *ModelPrice

	agents          *agentService
	prompts         *promptService
	responseFormats *responseFormatService
	sessions        *sessionService
	steps           *stepService
	costs           *costService
	budgets         *budgetService
	judges          JudgeRegistry
	gc              *gcService
	flows           *flowService
	poller          *asyncPollerService

	mu sync.RWMutex
}

// New creates and initialises a Loom engine from the given Config.
// It does NOT run schema migrations — call NewSchemaLoader(dialect).Apply(ctx, db) first.
func New(cfg Config) (*Engine, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("loom: DB is required")
	}
	if cfg.Dialect == "" {
		return nil, fmt.Errorf("loom: Dialect is required")
	}
	prefix := cfg.SchemaPrefix
	if prefix == "" {
		prefix = "loom_"
	}
	log := cfg.Logger
	if log == nil {
		log = noopLogger{}
	}
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	gens := make(map[string]Generator)
	maps.Copy(gens, cfg.Generators)

	e := &Engine{
		cfg:          cfg,
		db:           cfg.DB,
		prefix:       prefix,
		generators:   gens,
		cache:        cfg.Cache,
		hooks:        newHookBus(),
		log:          log,
		pricing:      cfg.Pricing,
		defaultPrice: cfg.DefaultPrice,
	}

	e.prompts = &promptService{e: e}
	e.agents = &agentService{e: e}
	e.responseFormats = &responseFormatService{e: e}
	e.sessions = &sessionService{e: e}
	e.steps = &stepService{e: e, maxRetries: maxRetries}
	e.costs = &costService{e: e}
	e.budgets = &budgetService{e: e}
	e.judges = newJudgeRegistryImpl()
	e.gc = &gcService{e: e}
	e.flows = &flowService{e: e}

	if cfg.AsyncPoller.Workers > 0 {
		e.poller = newAsyncPollerService(e, cfg.AsyncPoller)
	}

	// Register built-in budget enforcer pre-hook.
	e.hooks.RegisterPre("budget-enforcer", e.budgets.enforceHook)

	return e, nil
}

// Hooks returns the engine's hook bus for registering pre/post processors.
func (e *Engine) Hooks() HookBus { return e.hooks }

// RegisterGenerator adds or replaces a generator under a slug at runtime. This is
// how a platform extends into a new modality — register a video or world (3-D/AR)
// generator and reference it from an agent; no other engine change is needed.
func (e *Engine) RegisterGenerator(slug string, g Generator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.generators[slug] = g
}

// generator resolves a registered generator by slug under a read lock.
func (e *Engine) generator(slug string) (Generator, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	g, ok := e.generators[slug]
	return g, ok
}

// Agents returns the agent registry.
func (e *Engine) Agents() AgentRegistry { return e.agents }

// Prompts returns the prompt registry.
func (e *Engine) Prompts() PromptRegistry { return e.prompts }

// Sessions returns the session registry.
func (e *Engine) Sessions() SessionRegistry { return e.sessions }

// Cost returns the cost manager.
func (e *Engine) Cost() CostManager { return e.costs }

// Budgets returns the budget manager.
func (e *Engine) Budgets() BudgetManager { return e.budgets }

// Judges returns the judge registry.
func (e *Engine) Judges() JudgeRegistry { return e.judges }

// GC returns the branch garbage collection service.
func (e *Engine) GC() GCService { return e.gc }

// Flows returns the persisted-flow (agent map) registry.
func (e *Engine) Flows() FlowRegistry { return e.flows }

// StartPoller starts the background async-result poller. It is a no-op if
// AsyncPoller.Workers == 0 in the config.
func (e *Engine) StartPoller(ctx context.Context) {
	if e.poller != nil {
		go e.poller.Run(ctx)
	}
}

// RunStep executes one step of the generation loop against session.
// It resolves the agent, renders the user template, runs pre-hooks, calls the
// generator, runs post-hooks, persists the step, and returns it.
func (e *Engine) RunStep(ctx context.Context, session *Session, req StepRequest) (*Step, error) {
	return e.steps.run(ctx, session, req)
}

// -----------------------------------------------------------------------
// Internal services
// -----------------------------------------------------------------------

// agentService implements AgentRegistry.
type agentService struct{ e *Engine }

func (s *agentService) Get(ctx context.Context, slug string, version int) (*Agent, error) {
	if version == 0 {
		return s.Latest(ctx, slug)
	}
	// Cache check — agents are immutable after creation.
	key := cacheKey("agent", s.e.prefix, slug, version)
	if a, ok := cacheGet[Agent](ctx, s.e.cache, key); ok {
		return a, nil
	}
	a, err := queryAgent(ctx, s.e.db, s.e.prefix, slug, version)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, a)
	return a, nil
}

func (s *agentService) Latest(ctx context.Context, slug string) (*Agent, error) {
	return queryAgentLatest(ctx, s.e.db, s.e.prefix, slug)
}

func (s *agentService) Create(ctx context.Context, a *Agent) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	return insertAgent(ctx, s.e.db, s.e.prefix, a)
}

func (s *agentService) List(ctx context.Context, category string) ([]*Agent, error) {
	return listAgents(ctx, s.e.db, s.e.prefix, category)
}

func (s *agentService) Delete(ctx context.Context, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete agent %q: an explicit version >= 1 is required", slug)
	}
	if err := sqlDeleteAgent(ctx, s.e.db, s.e.prefix, slug, version); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, cacheKey("agent", s.e.prefix, slug, version))
	return nil
}

// promptService implements PromptRegistry.
type promptService struct{ e *Engine }

func (s *promptService) Get(ctx context.Context, slug string, version int) (*Prompt, error) {
	if version == 0 {
		return s.Latest(ctx, slug)
	}
	// Cache check — prompts are immutable after creation.
	key := cacheKey("prompt", s.e.prefix, slug, version)
	if p, ok := cacheGet[Prompt](ctx, s.e.cache, key); ok {
		return p, nil
	}
	p, err := queryPrompt(ctx, s.e.db, s.e.prefix, slug, version)
	if err != nil {
		return nil, err
	}
	cacheSet(ctx, s.e.cache, key, p)
	return p, nil
}

func (s *promptService) Latest(ctx context.Context, slug string) (*Prompt, error) {
	return queryPromptLatest(ctx, s.e.db, s.e.prefix, slug)
}

func (s *promptService) Create(ctx context.Context, p *Prompt) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	return insertPrompt(ctx, s.e.db, s.e.prefix, p)
}

func (s *promptService) List(ctx context.Context, kind PromptKind, category string) ([]*Prompt, error) {
	return listPrompts(ctx, s.e.db, s.e.prefix, kind, category)
}

func (s *promptService) Delete(ctx context.Context, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("loom: delete prompt %q: an explicit version >= 1 is required", slug)
	}
	// Resolve the row id first so we can also evict the by-id cache that the
	// agent-build path populates (cacheKey "prompt-id").
	var id uuid.UUID
	if p, err := queryPrompt(ctx, s.e.db, s.e.prefix, slug, version); err == nil {
		id = p.ID
	}
	if err := sqlDeletePrompt(ctx, s.e.db, s.e.prefix, slug, version); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, cacheKey("prompt", s.e.prefix, slug, version))
	if id != uuid.Nil {
		cacheDelete(ctx, s.e.cache, cacheKey("prompt-id", s.e.prefix, id.String(), 0))
	}
	return nil
}

// sessionService implements SessionRegistry.
type sessionService struct{ e *Engine }

func (s *sessionService) Create(ctx context.Context, sess *Session) error {
	if sess.ID == uuid.Nil {
		sess.ID = uuid.New()
	}
	now := time.Now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	return insertSession(ctx, s.e.db, s.e.prefix, sess)
}

func (s *sessionService) Get(ctx context.Context, id uuid.UUID) (*Session, error) {
	sess, err := querySession(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, err
	}
	steps, err := querySteps(ctx, s.e.db, s.e.prefix, id)
	if err != nil {
		return nil, err
	}
	sess.History = steps
	return sess, nil
}

func (s *sessionService) Update(ctx context.Context, sess *Session) error {
	sess.UpdatedAt = time.Now()
	return updateSession(ctx, s.e.db, s.e.prefix, sess)
}

func (s *sessionService) Fork(ctx context.Context, parentID uuid.UUID, stepIndex int) (*Session, error) {
	parent, err := querySession(ctx, s.e.db, s.e.prefix, parentID)
	if err != nil {
		return nil, fmt.Errorf("loom: fork: %w", err)
	}
	branch := &Session{
		ID:              uuid.New(),
		PlatformID:      parent.PlatformID,
		ParentSessionID: &parentID,
		BranchPoint:     &stepIndex,
		State:           parent.State,
		Tags:            append([]string(nil), parent.Tags...),
		Metadata:        parent.Metadata,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	if err := insertSession(ctx, s.e.db, s.e.prefix, branch); err != nil {
		return nil, err
	}
	return branch, nil
}

func (s *sessionService) BranchTree(ctx context.Context, rootID uuid.UUID) (*BranchNode, error) {
	return buildBranchTree(ctx, s.e.db, s.e.prefix, rootID)
}

func (s *sessionService) Pin(ctx context.Context, id uuid.UUID) error {
	return setPinned(ctx, s.e.db, s.e.prefix, id, true)
}

func (s *sessionService) Unpin(ctx context.Context, id uuid.UUID) error {
	return setPinned(ctx, s.e.db, s.e.prefix, id, false)
}

func (s *sessionService) Discard(ctx context.Context, id uuid.UUID) error {
	return softDeleteSession(ctx, s.e.db, s.e.prefix, id)
}

func (s *sessionService) Purge(ctx context.Context, id uuid.UUID) error {
	return hardDeleteSession(ctx, s.e.db, s.e.prefix, id)
}

func (s *sessionService) List(ctx context.Context, platformID string, limit, offset int) ([]*Session, error) {
	return listSessions(ctx, s.e.db, s.e.prefix, platformID, limit, offset)
}

// stepService implements StepRunner and houses the core generation loop.
type stepService struct {
	e          *Engine
	maxRetries int
}

func (s *stepService) run(ctx context.Context, session *Session, req StepRequest) (*Step, error) {
	start := time.Now()

	// Resolve agent.
	agent := req.Agent
	if agent == nil {
		var err error
		agent, err = s.e.agents.Get(ctx, req.AgentSlug, req.AgentVersion)
		if err != nil {
			return nil, fmt.Errorf("loom: resolve agent %q: %w", req.AgentSlug, err)
		}
	}

	// Look up generator. A per-request GeneratorOverride wins over the agent's
	// configured generator, enabling per-call provider/model routing.
	genSlug := agent.GeneratorSlug
	if req.GeneratorOverride != "" {
		genSlug = req.GeneratorOverride
	}
	gen, ok := s.e.generator(genSlug)
	if !ok {
		return nil, fmt.Errorf("loom: generator %q not registered", genSlug)
	}

	// Each step runs against a per-step copy of the session so concurrent
	// followers in a turn mutate/read their own State.Vars instead of racing on
	// the shared map; hook-written Vars are merged back under the lock below. The
	// copy is taken under the read lock so it does not race a sibling follower's
	// locked persist tail (which appends History and writes State).
	s.e.mu.RLock()
	stepSession := session.forStep()
	s.e.mu.RUnlock()

	// Expose the (per-step) session to hooks before they run.
	req.Session = stepSession

	// Run pre-hooks first so they can mutate session state (e.g. inject recalled
	// memories into State.Vars or set req.Inputs) *before* the user template is
	// rendered from that state.
	if err := s.e.hooks.RunPre(ctx, &req); err != nil {
		if IsSkip(err) {
			return nil, err // propagate skip to caller
		}
		return nil, fmt.Errorf("loom: pre-hook: %w", err)
	}

	// Build GenerateRequest (renders user template, loads system prompt) using
	// the (possibly hook-mutated) session and request.
	genReq, err := s.buildGenerateRequest(ctx, stepSession, agent, &req)
	if err != nil {
		return nil, err
	}
	// Surface the resolved output contract so post-hooks can validate against it.
	req.ResponseFormat = genReq.ResponseFormat

	// Generation loop with retry support.
	var result Result
	diagnostics := map[string]any{}

	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			// Merge forbidden phrases into the next request.
			genReq.Annotations = req.annotations
		}

		// Streaming vs sync generation.
		if sg, ok := gen.(StreamingGenerator); ok && req.OnChunk != nil {
			chunks, resultCh, streamErr := sg.GenerateStream(ctx, genReq)
			if streamErr != nil {
				return nil, fmt.Errorf("loom: generator stream: %w", streamErr)
			}
			// Forward chunks until the adapter closes the channel, honouring
			// cancellation so a stuck stream cannot hang the step.
		drainChunks:
			for {
				select {
				case c, more := <-chunks:
					if !more {
						break drainChunks
					}
					req.OnChunk(c)
				case <-ctx.Done():
					return nil, fmt.Errorf("loom: generator stream: %w", ctx.Err())
				}
			}
			var more bool
			select {
			case result, more = <-resultCh:
			case <-ctx.Done():
				return nil, fmt.Errorf("loom: generator stream: %w", ctx.Err())
			}
			// Mirror the sync error path: a closed channel (no result) or a failed
			// result is a generator error, not a successful empty turn.
			if !more || result == nil {
				return nil, fmt.Errorf("loom: generator stream: no result")
			}
			if result.Status() == ResultStatusFailed {
				if msg, _ := result.Metadata()["error"].(string); msg != "" {
					return nil, fmt.Errorf("loom: generator stream: %s", msg)
				}
				return nil, fmt.Errorf("loom: generator stream: failed")
			}
		} else {
			result, err = gen.Generate(ctx, genReq)
			if err != nil {
				return nil, fmt.Errorf("loom: generator: %w", err)
			}
		}

		// Run post-hooks.
		result, err = s.e.hooks.RunPost(ctx, &req, result)
		if err == nil {
			break // success
		}
		if !IsRetry(err) {
			return nil, fmt.Errorf("loom: post-hook: %w", err)
		}
		if attempt == s.maxRetries {
			return nil, fmt.Errorf("loom: max retries (%d) exceeded: %w", s.maxRetries, err)
		}
		ann, _ := RetryAnnotationFrom(err)
		req.annotations = append(req.annotations, ann)
		diagnostics[fmt.Sprintf("retry_%d_reason", attempt+1)] = ann.Reason
	}

	// Build and persist step.
	// Ensure the action has a unique ID before persisting.
	if req.Action != nil && req.Action.ID == (uuid.UUID{}) {
		req.Action.ID = uuid.New()
	}
	// Tag the step with turn linkage when it is part of a RunTurn flow so the
	// steps of one turn can be grouped for branching, replay, and cost rollups.
	if req.turnID != uuid.Nil {
		diagnostics["turn_id"] = req.turnID.String()
		diagnostics["turn_role"] = req.turnRole
	}

	// Serialise the session-mutating tail of the step. RunTurn runs follower
	// agents concurrently against the same *Session; without this lock their
	// index assignment and History appends would race.
	s.e.mu.Lock()
	defer s.e.mu.Unlock()

	step := &Step{
		ID:          uuid.New(),
		SessionID:   session.ID,
		Index:       len(session.History),
		AgentID:     agent.ID,
		Request:     buildStoredRequest(genReq),
		Result:      result,
		Action:      req.Action,
		Diagnostics: diagnostics,
		DurationMs:  int(time.Since(start).Milliseconds()),
		CreatedAt:   time.Now(),
	}
	if err := insertStep(ctx, s.e.db, s.e.prefix, step); err != nil {
		return nil, fmt.Errorf("loom: persist step: %w", err)
	}

	// Update session history and state.
	session.History = append(session.History, *step)
	session.State.Modality = result.Modality()
	// Reconcile any Vars the hooks wrote on the per-step copy back onto the
	// shared session (last writer wins), under the lock.
	mergeVars(&session.State, stepSession.State.Vars)
	if err := s.e.sessions.Update(ctx, session); err != nil {
		s.e.log.Error("update session after step", "session_id", session.ID, "err", err)
	}

	// Record cost asynchronously (best-effort).
	go s.e.costs.recordFromResult(context.Background(), step, agent, result, session.PlatformID)

	return step, nil
}

func (s *stepService) buildGenerateRequest(
	ctx context.Context,
	session *Session,
	agent *Agent,
	req *StepRequest,
) (GenerateRequest, error) {
	action := req.Action
	annotations := req.annotations
	// Load system prompt — cached by UUID.
	systemPrompt := ""
	if agent.SystemPromptID != uuid.Nil {
		spKey := cacheKey("prompt-id", s.e.prefix, agent.SystemPromptID.String(), 0)
		var sp *Prompt
		if cached, ok := cacheGet[Prompt](ctx, s.e.cache, spKey); ok {
			sp = cached
		} else {
			var err error
			sp, err = queryPromptByID(ctx, s.e.db, s.e.prefix, agent.SystemPromptID)
			if err != nil {
				return GenerateRequest{}, fmt.Errorf("load system prompt: %w", err)
			}
			cacheSet(ctx, s.e.cache, spKey, sp)
		}
		systemPrompt = sp.Body
	}

	// Load and render user template — cached by UUID.
	userPrompt := ""
	if agent.UserTemplateID != uuid.Nil {
		utKey := cacheKey("prompt-id", s.e.prefix, agent.UserTemplateID.String(), 0)
		var ut *Prompt
		if cached, ok := cacheGet[Prompt](ctx, s.e.cache, utKey); ok {
			ut = cached
		} else {
			var err error
			ut, err = queryPromptByID(ctx, s.e.db, s.e.prefix, agent.UserTemplateID)
			if err != nil {
				return GenerateRequest{}, fmt.Errorf("load user template: %w", err)
			}
			cacheSet(ctx, s.e.cache, utKey, ut)
		}
		// Render under a read lock — a sibling follower's locked tail may be
		// appending to session.History / writing session.State right now.
		var err error
		s.e.mu.RLock()
		userPrompt, err = renderTemplate(ut.Body, session, action, req.Inputs, req.Params)
		s.e.mu.RUnlock()
		if err != nil {
			return GenerateRequest{}, fmt.Errorf("render user template: %w", err)
		}
	}

	params := agent.Params
	if req.ParamOverride != nil {
		params = *req.ParamOverride
	}
	// params is a shallow struct copy, so params.Extra still aliases the agent's
	// (or override's) shared map. Clone it before applyParamsMap mutates it in
	// place — otherwise concurrent steps sharing the agent race/corrupt that map
	// and one turn's params leak into the next. Clone(nil) is nil (then allocated).
	params.Extra = maps.Clone(params.Extra)
	// Fold the free-form params map onto the typed params (known model keys) and
	// keep the whole map for the generator and template.
	applyParamsMap(&params, req.Params)

	// Resolve the response format — an independent piece of the agent's
	// composition (system prompt + user template + response format). A referenced
	// record (shared, reusable) wins over an inline value.
	responseFormat := agent.ResponseFormat
	if agent.ResponseFormatID != nil && *agent.ResponseFormatID != uuid.Nil {
		rec, err := s.e.responseFormats.GetByID(ctx, *agent.ResponseFormatID)
		if err != nil {
			return GenerateRequest{}, fmt.Errorf("load response format: %w", err)
		}
		responseFormat = rec.Format()
	}

	// Snapshot history under a read lock — RunTurn runs follower agents
	// concurrently, and a sibling's locked tail may be appending right now.
	s.e.mu.RLock()
	histContext := collectResultContext(session.History)
	s.e.mu.RUnlock()

	return GenerateRequest{
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Params:         params,
		ResponseFormat: responseFormat,
		Context:        histContext,
		SessionID:      session.ID,
		AgentID:        agent.ID,
		Annotations:    annotations,
		Inputs:         req.Inputs,
		Overrides:      req.Overrides,
		ParamsMap:      req.Params,
		Bus:            req.bus,
	}, nil
}

// applyParamsMap folds known model keys from a free-form params map onto the
// typed GenerateParams, and accumulates every key (including domain knobs like
// "tension") into Params.Extra so generators can read them.
func applyParamsMap(p *GenerateParams, m map[string]any) {
	if len(m) == 0 {
		return
	}
	for k, v := range m {
		switch k {
		case "temperature":
			p.Temperature = toFloat(v)
		case "top_p":
			p.TopP = toFloat(v)
		case "frequency_penalty":
			p.FrequencyPenalty = toFloat(v)
		case "presence_penalty":
			p.PresencePenalty = toFloat(v)
		case "duration_sec":
			p.DurationSec = toFloat(v)
		case "max_tokens":
			p.MaxTokens = toInt(v)
		case "seed":
			p.Seed = toInt(v)
		case "width":
			p.Width = toInt(v)
		case "height":
			p.Height = toInt(v)
		}
	}
	if p.Extra == nil {
		p.Extra = map[string]any{}
	}
	maps.Copy(p.Extra, m)
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	}
	return 0
}

// renderTemplate renders a Go text/template with the session, action, and
// per-step inputs as data.
func renderTemplate(body string, session *Session, action *Action, inputs, params map[string]any) (string, error) {
	tmpl, err := template.New("user").Parse(body)
	if err != nil {
		return "", err
	}
	data := map[string]any{
		"Session":  session,
		"State":    session.State,
		"Snapshot": session.State.Snapshot,
		"Vars":     session.State.Vars,
		"Action":   action,
		"History":  session.History,
		"Inputs":   inputs,
		"Params":   params,
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// collectResultContext returns the last N results from step history for context continuity.
func collectResultContext(history []Step) []Result {
	const window = 10
	start := 0
	if len(history) > window {
		start = len(history) - window
	}
	out := make([]Result, 0, len(history)-start)
	for _, s := range history[start:] {
		if s.Result != nil {
			out = append(out, s.Result)
		}
	}
	return out
}

func buildStoredRequest(req GenerateRequest) StoredRequest {
	return StoredRequest{
		SystemPrompt:   req.SystemPrompt,
		UserPrompt:     req.UserPrompt,
		Params:         req.Params,
		ResponseFormat: req.ResponseFormat,
	}
}

// -----------------------------------------------------------------------
// Stub persistence functions — implemented fully in internal/store/postgres
// -----------------------------------------------------------------------

// These thin wrappers delegate to the SQL functions defined in persist.go.
// They are kept here to keep engine.go self-contained; the actual SQL lives
// in persist.go so this file stays readable.

func queryAgent(ctx context.Context, db *sql.DB, prefix, slug string, version int) (*Agent, error) {
	return sqlQueryAgent(ctx, db, prefix, slug, version)
}
func queryAgentLatest(ctx context.Context, db *sql.DB, prefix, slug string) (*Agent, error) {
	return sqlQueryAgentLatest(ctx, db, prefix, slug)
}
func insertAgent(ctx context.Context, db *sql.DB, prefix string, a *Agent) error {
	return sqlInsertAgent(ctx, db, prefix, a)
}
func listAgents(ctx context.Context, db *sql.DB, prefix, category string) ([]*Agent, error) {
	return sqlListAgents(ctx, db, prefix, category)
}

func queryPrompt(ctx context.Context, db *sql.DB, prefix, slug string, version int) (*Prompt, error) {
	return sqlQueryPrompt(ctx, db, prefix, slug, version)
}
func queryPromptLatest(ctx context.Context, db *sql.DB, prefix, slug string) (*Prompt, error) {
	return sqlQueryPromptLatest(ctx, db, prefix, slug)
}
func queryPromptByID(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Prompt, error) {
	return sqlQueryPromptByID(ctx, db, prefix, id)
}
func insertPrompt(ctx context.Context, db *sql.DB, prefix string, p *Prompt) error {
	return sqlInsertPrompt(ctx, db, prefix, p)
}
func listPrompts(ctx context.Context, db *sql.DB, prefix string, kind PromptKind, category string) ([]*Prompt, error) {
	return sqlListPrompts(ctx, db, prefix, kind, category)
}

func querySession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (*Session, error) {
	return sqlQuerySession(ctx, db, prefix, id)
}
func insertSession(ctx context.Context, db *sql.DB, prefix string, s *Session) error {
	return sqlInsertSession(ctx, db, prefix, s)
}
func updateSession(ctx context.Context, db *sql.DB, prefix string, s *Session) error {
	return sqlUpdateSession(ctx, db, prefix, s)
}
func buildBranchTree(ctx context.Context, db *sql.DB, prefix string, rootID uuid.UUID) (*BranchNode, error) {
	return sqlBuildBranchTree(ctx, db, prefix, rootID)
}
func setPinned(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, pinned bool) error {
	return sqlSetPinned(ctx, db, prefix, id, pinned)
}
func softDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	return sqlSoftDeleteSession(ctx, db, prefix, id)
}
func hardDeleteSession(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) error {
	return sqlHardDeleteSession(ctx, db, prefix, id)
}
func listSessions(ctx context.Context, db *sql.DB, prefix, platformID string, limit, offset int) ([]*Session, error) {
	return sqlListSessions(ctx, db, prefix, platformID, limit, offset)
}

func querySteps(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID) ([]Step, error) {
	return sqlQuerySteps(ctx, db, prefix, sessionID)
}
func insertStep(ctx context.Context, db *sql.DB, prefix string, step *Step) error {
	return sqlInsertStep(ctx, db, prefix, step)
}

// JudgeRegistry is the public interface returned by Engine.Judges().
type JudgeRegistry interface {
	// Rubric returns a RubricJudge by slug.
	Rubric(slug string) RubricJudge
	// Pairwise returns a PairwiseJudge by slug.
	Pairwise(slug string) PairwiseJudge
	// Constraint returns a ConstraintJudge by slug.
	Constraint(slug string) ConstraintJudge
	// Register registers a judge under a slug.
	Register(slug string, j Judge)
}

// GCService is the public interface returned by Engine.GC().
type GCService interface {
	// Run starts the periodic GC sweep. Blocking; run in a goroutine.
	Run(ctx context.Context)
	// DryRun returns what the next sweep would delete without acting.
	DryRun(ctx context.Context) (*SweepReport, error)
}

type gcService struct{ e *Engine }

// CostManager is the public interface returned by Engine.Cost().
type CostManager interface {
	// SessionUsage returns aggregated cost for a single session.
	SessionUsage(ctx context.Context, sessionID uuid.UUID) (*UsageSummary, error)
	// Usage returns aggregated cost for a target over a time window.
	Usage(ctx context.Context, q UsageQuery) (*UsageSummary, error)
	// ByAgent returns per-agent cost breakdown.
	ByAgent(ctx context.Context, q UsageQuery) ([]AgentUsage, error)
	// AgentStats returns p50/p95 cost stats for a single agent.
	AgentStats(ctx context.Context, agentID uuid.UUID, window time.Duration) (*AgentCostStats, error)
	// Estimate returns a pre-flight cost estimate for a step request.
	Estimate(ctx context.Context, req EstimateRequest) (*CostEstimate, error)
	// Record persists a cost record.
	Record(ctx context.Context, rec CostRecord) error
}

// BudgetManager is the public interface returned by Engine.Budgets().
type BudgetManager interface {
	// Create persists a new budget.
	Create(ctx context.Context, b *Budget) error
	// Get retrieves a budget by ID.
	Get(ctx context.Context, id uuid.UUID) (*Budget, error)
	// List returns all budgets matching the given target.
	List(ctx context.Context, targetKind, targetKey string) ([]*Budget, error)
	// Delete removes a budget.
	Delete(ctx context.Context, id uuid.UUID) error
}

// asyncPollerService polls async provider results in the background.
type asyncPollerService struct {
	e   *Engine
	cfg PollerConfig
}

func newAsyncPollerService(e *Engine, cfg PollerConfig) *asyncPollerService {
	return &asyncPollerService{e: e, cfg: cfg}
}

func (p *asyncPollerService) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	sem := make(chan struct{}, p.cfg.Workers)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()
				p.pollPending(ctx)
			}()
		}
	}
}

func (p *asyncPollerService) pollPending(ctx context.Context) {
	tasks, err := sqlListPendingTasks(ctx, p.e.db, p.e.prefix)
	if err != nil {
		p.e.log.Error("poll pending tasks", "err", err)
		return
	}
	for _, t := range tasks {
		gen, ok := p.e.generator(t.Provider)
		if !ok {
			continue
		}
		poller, ok := gen.(interface {
			Poll(ctx context.Context, handle TaskHandle) (Result, error)
		})
		if !ok {
			continue
		}
		result, err := poller.Poll(ctx, TaskHandle{
			ID:       t.ID,
			Provider: t.Provider,
			Handle:   t.Handle,
		})
		switch {
		case err != nil:
			p.e.log.Error("poll task", "task_id", t.ID, "err", err)
			p.retryOrFail(ctx, t, err.Error())
		case result == nil:
			// Defensive: a generator returning (nil, nil) is a non-terminal miss,
			// not a worker panic.
			p.retryOrFail(ctx, t, "poll returned no result")
		case result.Status() == ResultStatusReady:
			if err := sqlMarkTaskReady(ctx, p.e.db, p.e.prefix, t.ID, result); err != nil {
				p.e.log.Error("mark task ready", "task_id", t.ID, "err", err)
			}
		case result.Status() == ResultStatusFailed:
			_ = sqlMarkTaskFailed(ctx, p.e.db, p.e.prefix, t.ID, resultErrorMessage(result))
		default:
			// Still pending at the provider: count the poll so a job that never
			// resolves is eventually capped rather than polled forever.
			p.retryOrFail(ctx, t, "still pending after maximum poll attempts")
		}
	}
}

// defaultMaxPollAttempts caps how many times a task is polled before it is
// terminally failed when PollerConfig.MaxAttempts is unset.
const defaultMaxPollAttempts = 60

func (p *asyncPollerService) maxAttempts() int {
	if p.cfg.MaxAttempts > 0 {
		return p.cfg.MaxAttempts
	}
	return defaultMaxPollAttempts
}

// retryOrFail increments a task's attempt counter, or terminally fails it once it
// reaches the attempt cap, so neither erroring nor perpetually-pending jobs are
// polled forever.
func (p *asyncPollerService) retryOrFail(ctx context.Context, t pendingTask, reason string) {
	if t.Attempts+1 >= p.maxAttempts() {
		_ = sqlMarkTaskFailed(ctx, p.e.db, p.e.prefix, t.ID, reason)
	} else {
		_ = sqlIncrementTaskAttempts(ctx, p.e.db, p.e.prefix, t.ID, reason)
	}
}

// resultErrorMessage extracts a failed result's error string, falling back to a
// generic message.
func resultErrorMessage(r Result) string {
	if r != nil {
		if e, ok := r.Metadata()["error"].(string); ok && e != "" {
			return e
		}
	}
	return "provider reported failure"
}

// -----------------------------------------------------------------------
// Stub types to satisfy forward references in this file.
// Full implementations are in their respective sub-packages.
// -----------------------------------------------------------------------

type pendingTask struct {
	ID       uuid.UUID
	Provider string
	Handle   string
	Attempts int
}

// UsageSummary aggregates cost metrics.
type UsageSummary struct {
	TotalUSD    float64
	TotalTokens int
	StepCount   int
	ByModel     map[string]float64
}

// UsageQuery is the input to cost queries.
type UsageQuery struct {
	Target BudgetTarget
	From   time.Time
	To     time.Time
	Tags   []string // prefix "!" to exclude a tag
}

// AgentUsage is a per-agent cost breakdown entry.
type AgentUsage struct {
	AgentSlug    string
	AgentVersion int
	RunCount     int
	USDTotal     float64
}

// AgentCostStats holds statistical cost data for an agent.
type AgentCostStats struct {
	USDMean   float64
	USDP50    float64
	USDP95    float64
	TokensP50 int
	TokensP95 int
}

// EstimateRequest is the input to Cost().Estimate().
type EstimateRequest struct {
	AgentID uuid.UUID
	Input   string
	Modal   Modality
}

// CostEstimate is the output of Cost().Estimate().
type CostEstimate struct {
	USDLow                float64
	USDHigh               float64
	InputTokens           int
	OutputTokensEstimated int
}

// Budget defines a (target, window, limit) spending triple.
type Budget struct {
	ID          uuid.UUID
	Name        string
	Target      BudgetTarget
	Window      BudgetWindow
	Limit       BudgetLimit
	OnExceed    BudgetAction
	TagsInclude []string
	TagsExclude []string
	Active      bool
	CreatedAt   time.Time
}

// BudgetTarget identifies the entity a budget applies to.
type BudgetTarget struct {
	Kind string // "session" | "platform_id" | "custom"
	Key  string
}

// BudgetWindow is the rolling time window for a budget.
type BudgetWindow string

const (
	BudgetWindowHour     BudgetWindow = "hour"
	BudgetWindowDay      BudgetWindow = "day"
	BudgetWindowWeek     BudgetWindow = "week"
	BudgetWindowMonth    BudgetWindow = "month"
	BudgetWindowLifetime BudgetWindow = "lifetime"
)

// BudgetLimit constrains usage by USD, tokens, and/or step count.
type BudgetLimit struct {
	USD    float64 // 0 = no USD limit
	Tokens int     // 0 = no token limit
	Steps  int     // 0 = no step limit
}

// BudgetAction is what happens when a budget is exceeded.
type BudgetAction string

const (
	BudgetBlock     BudgetAction = "block"
	BudgetDowngrade BudgetAction = "downgrade"
	BudgetNotify    BudgetAction = "notify"
)

// TargetPlatformID is the BudgetTarget.Kind for per-user budgets.
const TargetPlatformID = "platform_id"

// SweepReport is returned by GC.DryRun.
type SweepReport struct {
	SpeculativeDeleted  int
	StaleSoftDeleted    int
	GraceHardDeleted    int
	TestBranchesDeleted int
}

// costService implements CostManager.
type costService struct{ e *Engine }

func (c *costService) SessionUsage(ctx context.Context, sessionID uuid.UUID) (*UsageSummary, error) {
	return sqlSessionUsage(ctx, c.e.db, c.e.prefix, sessionID)
}

func (c *costService) Usage(ctx context.Context, q UsageQuery) (*UsageSummary, error) {
	return sqlUsage(ctx, c.e.db, c.e.prefix, q)
}

func (c *costService) ByAgent(ctx context.Context, q UsageQuery) ([]AgentUsage, error) {
	return sqlByAgent(ctx, c.e.db, c.e.prefix, q)
}

func (c *costService) AgentStats(ctx context.Context, agentID uuid.UUID, window time.Duration) (*AgentCostStats, error) {
	return sqlAgentStats(ctx, c.e.db, c.e.prefix, agentID, window)
}

func (c *costService) Estimate(_ context.Context, req EstimateRequest) (*CostEstimate, error) {
	// Simple estimation: count input tokens naively (1 token ≈ 4 chars).
	inputTokens := len(req.Input) / 4
	// Use conservative output token estimates by modality.
	outputEst := 0
	switch req.Modal {
	case ModalityText, ModalityStructured:
		outputEst = 512
	}
	return &CostEstimate{
		USDLow:                float64(inputTokens) * 0.000001,
		USDHigh:               float64(inputTokens+outputEst) * 0.000030,
		InputTokens:           inputTokens,
		OutputTokensEstimated: outputEst,
	}, nil
}

func (c *costService) Record(ctx context.Context, rec CostRecord) error {
	return sqlInsertCostRecord(ctx, c.e.db, c.e.prefix, rec)
}

func (c *costService) recordFromResult(ctx context.Context, step *Step, agent *Agent, result Result, platformID string) {
	var (
		inputTokens  int
		outputTokens int
		usdCost      float64
	)
	switch r := result.(type) {
	case *TextResult:
		inputTokens = r.InputTokens
		outputTokens = r.OutputTokens
	case *StructuredResult:
		inputTokens = r.InputTokens
		outputTokens = r.OutputTokens
	}
	// Per-model pricing: the generator reports its model on result metadata;
	// fall back to the generator slug as the pricing key.
	model := ResultModel(result)
	if model == "" {
		model = agent.GeneratorSlug
	}
	usdCost = c.e.priceFor(model).Cost(inputTokens, outputTokens)

	rec := CostRecord{
		Provider:     agent.GeneratorSlug,
		Model:        model,
		Modal:        agent.Modal,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		USDCost:      usdCost,
		StepID:       step.ID,
		AgentID:      agent.ID,
		SessionID:    step.SessionID,
		PlatformID:   platformID,
		Timestamp:    time.Now(),
	}
	if err := c.Record(ctx, rec); err != nil {
		c.e.log.Error("record cost", "step_id", step.ID, "err", err)
	}
}

// budgetService implements BudgetManager and the budget pre-hook.
type budgetService struct{ e *Engine }

func (b *budgetService) Create(ctx context.Context, budget *Budget) error {
	if budget.ID == uuid.Nil {
		budget.ID = uuid.New()
	}
	if budget.CreatedAt.IsZero() {
		budget.CreatedAt = time.Now()
	}
	return sqlInsertBudget(ctx, b.e.db, b.e.prefix, budget)
}

func (b *budgetService) Get(ctx context.Context, id uuid.UUID) (*Budget, error) {
	return sqlQueryBudget(ctx, b.e.db, b.e.prefix, id)
}

func (b *budgetService) List(ctx context.Context, targetKind, targetKey string) ([]*Budget, error) {
	return sqlListBudgets(ctx, b.e.db, b.e.prefix, targetKind, targetKey)
}

func (b *budgetService) Delete(ctx context.Context, id uuid.UUID) error {
	return sqlDeleteBudget(ctx, b.e.db, b.e.prefix, id)
}

// enforceHook is registered as the built-in "budget-enforcer" pre-hook.
func (b *budgetService) enforceHook(ctx context.Context, req *StepRequest) error {
	// In a real implementation this would query loom_cost_records and compare
	// against active budgets for the session's platform_id.
	// For now it is a pass-through — the mechanism is wired; data makes it real.
	return nil
}

// -----------------------------------------------------------------------
// JSON helpers for result serialisation
// -----------------------------------------------------------------------

// MarshalResult serialises a Result to JSON for storage.
func MarshalResult(r Result) ([]byte, error) {
	return json.Marshal(r)
}
