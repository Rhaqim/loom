// Package engine is the internal implementation of the loom procgen engine:
// the Engine hub, its registry services, step/turn execution, hooks, cost and
// budget tracking, branch/replay, the async poller, and SQL persistence.
//
// It sits behind the module's internal/ boundary — external modules cannot
// import it. The public, stable surface is the root package
// github.com/rhaqim/loom, which re-exports the types and constructors here via
// type aliases. Application code should depend on that facade, never on this
// package directly.
package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"sync"
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
	// CacheKeyPrefix is the leading namespace on every cache key loom writes
	// (default "loom"). Set it when the Cache is shared with the application's
	// own keys so loom's entries cannot collide with theirs — e.g. "myapp:loom".
	// The schema prefix is always appended after it, so distinct engines stay
	// distinct regardless.
	CacheKeyPrefix string
	// CacheTTL is how long cached immutable objects (agents, prompts) live.
	// Zero uses the default (24h). Since these records are versioned and never
	// mutated, a long TTL is safe; set a shorter one only if your cache backend
	// requires eviction.
	CacheTTL time.Duration
	// LatestCacheTTL is how long a "latest/active" resolution is cached — the
	// hot-path reads Latest / LatestActive (and Get / RunTurnBySlug with
	// version 0) make when running at the current published version rather than
	// a pinned one. Unlike CacheTTL's targets this pointer is mutable, so it is
	// evicted on every Create / SetActive; the TTL is only a backstop for an
	// eviction that cannot reach another engine instance's cache.
	//
	// Zero uses the default (5s). A NEGATIVE value disables latest/active
	// caching while leaving versioned caching on — set it when even seconds of
	// cross-instance staleness after a publish is unacceptable. Only consulted
	// when Cache is non-nil.
	LatestCacheTTL time.Duration
	// Pricing maps model id → per-million-token price for accurate cost tracking.
	// Generators report their model on Result metadata ("model"); a model not in
	// the table falls back to DefaultPrice. Nil disables per-model pricing (the
	// flat default applies).
	Pricing map[string]ModelPrice
	// DefaultPrice is used for models absent from Pricing (default: $1/$3 per 1M).
	DefaultPrice *ModelPrice
	// JobPricing maps a generator slug or reported model id → USD per
	// generation, for providers that bill per job rather than per token (image,
	// video, audio and 3D generators). The model id is tried first, then the
	// generator slug. A job is charged when it is submitted, which is when most
	// providers bill, so an async job counts toward budgets while it is still
	// pending. A tokenless result with no entry here is recorded at $0 and
	// marked Estimated, since its real cost is unknown.
	JobPricing map[string]float64
	// BranchGC configures the branch garbage collector used by Engine.GC().
	// Nil uses safe defaults (DryRun=true, so nothing is deleted); set an explicit
	// config with DryRun=false to actually sweep. The Dialect is filled in from
	// Config.Dialect automatically.
	BranchGC *BranchGCConfig
	// PromptFileRoot enables file-backed prompts (PromptRef.File / PromptFromFile),
	// confined to this directory. It is empty by default, which DISABLES file
	// loading entirely: an unconfined os.ReadFile of a caller-supplied path is an
	// arbitrary-file-read primitive that exfiltrates the file's contents to the
	// LLM. When set, a PromptRef.File must resolve (after symlink evaluation) to a
	// path inside this root, or the read is rejected.
	PromptFileRoot string
	// Evaluator enables the EXPERIMENTAL evaluation subsystem: a decision
	// primitive that answers typed questions (yes/no, pick-one, rate) about a
	// state as calibrated probabilities, as the counterpart to a Generator's
	// content production. See internal/engine/api_evaluator.go and the
	// evaluator package.
	//
	// It is entirely opt-in. Nil — the default — leaves every engine code path
	// exactly as it was: Engine.Evaluate returns ErrEvaluatorNotConfigured, no
	// gate or hook consults an evaluator, and judges keep whatever backing the
	// application registered. Set it to opt in; the engine still calls it only
	// where the application wires it (EvalGate, FlowAgent.When, or an
	// evaluator-backed judge).
	Evaluator Evaluator

	// Broadcaster carries session events (Engine.Subscribe) to subscribers.
	// Nil uses an in-process broadcaster, which only reaches subscribers of
	// this engine instance; with several instances, supply a shared one.
	Broadcaster Broadcaster
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
	cfg            Config
	db             *sql.DB
	prefix         string
	generators     map[string]Generator
	hooks          *hookBus
	log            Logger
	cache          Cache
	cacheTTL       time.Duration
	cacheKeyPrefix string
	// latestCacheTTL is the resolved TTL for mutable latest/active pointers.
	// A negative value means latest/active caching is disabled.
	latestCacheTTL time.Duration
	pricing        map[string]ModelPrice
	defaultPrice   *ModelPrice
	jobPricing     map[string]float64
	broadcaster    Broadcaster
	// evaluator backs the experimental evaluation subsystem; nil when the
	// application has not opted in. Guarded by mu so SetEvaluator can replace it
	// at runtime alongside a running turn, as RegisterGenerator does.
	evaluator Evaluator

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

	onTaskResolved func(context.Context, TaskResolution)

	// pollerCancel stops the async poller; bg tracks every background goroutine
	// the engine owns so Close can wait for them to drain. Both are guarded by
	// mu.
	pollerCancel context.CancelFunc
	bg           sync.WaitGroup

	mu sync.RWMutex
}

// New creates and initialises a Loom engine from the given Config.
// It does NOT run schema migrations — call NewSchemaLoader(dialect).Apply(ctx, db) first.
func New(cfg Config) (*Engine, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("%w: DB is required", ErrInvalidConfig)
	}
	if cfg.Dialect == "" {
		return nil, fmt.Errorf("%w: Dialect is required", ErrInvalidConfig)
	}
	prefix := cfg.SchemaPrefix
	if prefix == "" {
		prefix = "loom_"
	}
	// SchemaPrefix is interpolated directly into table names in every query, so
	// it must be a safe SQL identifier. It is a config value (never request
	// data), but validate it as defense-in-depth so it can never become an
	// injection vector if an embedder ever sources it from untrusted input.
	if !validSchemaPrefix(prefix) {
		return nil, fmt.Errorf("%w: SchemaPrefix %q must match [A-Za-z0-9_]*", ErrInvalidConfig, prefix)
	}
	log := cfg.Logger
	if log == nil {
		log = noopLogger{}
	}
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	cacheTTL := cfg.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}
	// Zero means "use the default"; a negative value is preserved as the
	// disable signal (see cachedLatest).
	latestCacheTTL := cfg.LatestCacheTTL
	if latestCacheTTL == 0 {
		latestCacheTTL = defaultLatestCacheTTL
	}
	cacheKeyPrefix := cfg.CacheKeyPrefix
	if cacheKeyPrefix == "" {
		cacheKeyPrefix = defaultCacheKeyPrefix
	}
	gens := make(map[string]Generator)
	maps.Copy(gens, cfg.Generators)

	e := &Engine{
		cfg:            cfg,
		db:             cfg.DB,
		prefix:         prefix,
		generators:     gens,
		cache:          cfg.Cache,
		cacheTTL:       cacheTTL,
		cacheKeyPrefix: cacheKeyPrefix,
		latestCacheTTL: latestCacheTTL,
		hooks:          newHookBus(),
		log:            log,
		pricing:        cfg.Pricing,
		defaultPrice:   cfg.DefaultPrice,
		jobPricing:     cfg.JobPricing,
		evaluator:      cfg.Evaluator,
		broadcaster:    cfg.Broadcaster,
	}
	if e.broadcaster == nil {
		e.broadcaster = newMemoryBroadcaster()
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

	return e, nil
}

// validSchemaPrefix reports whether s is a safe table-name prefix — letters,
// digits, and underscores only (the empty string is allowed and defaulted).
func validSchemaPrefix(s string) bool {
	for _, r := range s {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
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

// Generators returns the slugs of the generators registered in THIS process,
// sorted. It reflects in-process registration only (built-ins plus any
// RegisterGenerator calls), not any database table — use it to validate an
// agent's GeneratorSlug before saving.
func (e *Engine) Generators() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	slugs := make([]string, 0, len(e.generators))
	for slug := range e.generators {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

// generator resolves a registered generator by slug under a read lock.
func (e *Engine) generator(slug string) (Generator, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	g, ok := e.generators[slug]
	return g, ok
}

// TaskResolution describes the terminal outcome of an async provider job,
// delivered to the callback registered with OnTaskResolved.
type TaskResolution struct {
	TaskID    uuid.UUID
	SessionID uuid.UUID
	AgentID   uuid.UUID
	Provider  string
	Status    ResultStatus // ResultStatusReady or ResultStatusFailed
	Result    Result       // the resolved asset on success; nil on failure
	Err       string       // failure reason when Status is failed
}

// OnTaskResolved registers a callback fired when an async task resolves — ready
// or terminally failed. It runs in the poller goroutine, so it must be fast and
// non-blocking; offload real work. Only one callback is held; a later call
// replaces it. Embedders use this to write a resolved asset into their own
// tables without reaching into loom's internal task tables.
func (e *Engine) OnTaskResolved(fn func(context.Context, TaskResolution)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onTaskResolved = fn
}

func (e *Engine) taskResolvedHook() func(context.Context, TaskResolution) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.onTaskResolved
}

// TaskStatus reports a task's status ("pending", "ready", or "failed") and, when
// ready, its resolved result. It lets an embedder check on an async job without
// querying loom's internal tables. Returns ErrNotFound when no such task exists.
func (e *Engine) TaskStatus(ctx context.Context, taskID uuid.UUID) (string, Result, error) {
	return sqlTaskStatus(ctx, e.db, e.prefix, taskID)
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
//
// The poller stops when ctx is cancelled OR when Close is called, whichever
// happens first. Prefer Close: it also WAITS for in-flight polls to finish,
// whereas cancelling ctx alone returns immediately and leaves stragglers
// writing to the database.
func (e *Engine) StartPoller(ctx context.Context) {
	if e.poller == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pollerCancel != nil {
		return // already started; starting twice would orphan the first poller
	}
	pollCtx, cancel := context.WithCancel(ctx)
	e.pollerCancel = cancel
	e.bg.Add(1)
	go func() {
		defer e.bg.Done()
		e.poller.Run(pollCtx)
	}()
}

// Close shuts the engine's background work down and waits for it to finish.
// It stops the async poller and blocks until every in-flight poll has
// returned, so that once Close returns the engine is no longer touching the
// database.
//
// Call it before dropping a schema, closing the *sql.DB, or exiting on
// SIGTERM. Without it a poll in flight races teardown — it either fails
// against tables that no longer exist, or lands a write after shutdown was
// believed complete. (Step costs are not at risk: they commit inside the step
// transaction, so a step is never durable without its cost row.)
//
// Close does NOT close the *sql.DB — the caller supplied it and may still be
// using it. It is safe to call more than once and safe on an engine whose
// poller was never started. If ctx expires before the drain completes, Close
// returns ctx.Err() and the stragglers are left running.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	cancel := e.pollerCancel
	e.pollerCancel = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		e.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunStep executes one step of the generation loop against session.
// It resolves the agent, renders the user template, runs pre-hooks, calls the
// generator, runs post-hooks, persists the step, and returns it.
//
// Errors (match with errors.Is / errors.As):
//   - ErrSkip — a pre-hook cancelled the step (use IsSkip); not a failure.
//   - *NotFoundError (unwraps to ErrNotFound) — the agent slug/version, or a
//     referenced prompt/response-format, does not exist.
//   - ErrGeneratorNotRegistered — the agent's generator slug is not registered.
//   - *BudgetExceededError — a configured budget blocked the step.
//   - *GenerationError — the generator failed; switch on Kind (transport /
//     rejected / empty).
//   - ErrSessionNotPersisted — PARTIAL SUCCESS. The step ran and committed, and
//     the returned *Step is non-nil and valid; only the loom_sessions row
//     update failed, so session.State in the database (and any in-memory
//     *Session) is stale. Callers using loom as a system of record must
//     reconcile — StateAt(ctx, id, step.Index) returns the authoritative
//     post-step state — before trusting session state. This is the one error
//     for which the returned *Step is non-nil.
//   - otherwise, a wrapped error from a hook, template render, or persistence.
func (e *Engine) RunStep(ctx context.Context, session *Session, req StepRequest) (*Step, error) {
	return e.steps.run(ctx, session, req)
}

// AddParticipant adds p to the session's participants and persists the
// session. JoinedAt defaults to now. Adding an ID that is already present
// returns ErrParticipantExists. Pass the same *Session the app runs steps on:
// the change is serialised with in-flight steps, so it is neither lost nor
// reported as a conflict. Membership is part of State, so the next step's
// checkpoint records it and a fork from before the join drops it.
//
// These live on Engine rather than SessionRegistry so that interface — which
// applications may implement themselves, e.g. as a test double — is unchanged.
func (e *Engine) AddParticipant(ctx context.Context, s *Session, p Participant) error {
	return e.sessions.AddParticipant(ctx, s, p)
}

// RemoveParticipant removes the participant with id and persists the session;
// an unknown id returns ErrNotParticipant.
func (e *Engine) RemoveParticipant(ctx context.Context, s *Session, id string) error {
	return e.sessions.RemoveParticipant(ctx, s, id)
}

// VerifyGenerator checks the registered generator slug's credentials and model
// (with optional per-request overrides such as a user's own "api_key" and
// "model") by asking the provider, without generating. It returns
// ErrGeneratorNotRegistered for an unknown slug, ErrVerifyNotSupported when the
// generator cannot verify, and an error wrapping ErrKeyRejected when the
// provider refuses the key.
func (e *Engine) VerifyGenerator(ctx context.Context, slug string, overrides map[string]any) (ModelInfo, error) {
	gen, ok := e.generator(slug)
	if !ok {
		return ModelInfo{}, fmt.Errorf("generator %q: %w", slug, ErrGeneratorNotRegistered)
	}
	v, ok := gen.(Verifier)
	if !ok {
		return ModelInfo{}, fmt.Errorf("generator %q: %w", slug, ErrVerifyNotSupported)
	}
	return v.Verify(ctx, overrides)
}

// Subscribe streams a session's events: first every persisted step from
// opts.FromStep on, replayed as step.done (Replayed = true), then live events
// as they happen — step.started, chunk, step.done, step.failed, turn.done and
// forked. A step that completes while the replay is being read is delivered
// once. Chunks are live only.
//
// To resume after a disconnect, subscribe again with FromStep one past the
// last step.done StepIndex received. Close the subscription (or cancel ctx)
// when done. A consumer that falls more than a few hundred events behind is
// disconnected and Err returns ErrSubscriberLagged.
//
// Live events reach only subscribers of this engine instance unless
// Config.Broadcaster is a shared broadcaster.
func (e *Engine) Subscribe(ctx context.Context, sessionID uuid.UUID, opts SubscribeOptions) (*Subscription, error) {
	if _, err := querySession(ctx, e.db, e.prefix, sessionID); err != nil {
		return nil, err
	}
	subCtx, cancel := context.WithCancel(ctx)
	// Subscribe to live events BEFORE reading history, so a step that lands
	// in between is not missed (it is de-duplicated below instead).
	live, lagged, liveCancel := e.broadcaster.Subscribe(subCtx, sessionID)
	var replay []Step
	if opts.FromStep >= 0 {
		var err error
		if replay, err = sqlQueryStepsFrom(subCtx, e.db, e.prefix, sessionID, opts.FromStep); err != nil {
			liveCancel()
			cancel()
			return nil, err
		}
	}
	out := make(chan SessionEvent, 64)
	var (
		mu      sync.Mutex
		lastErr error
	)
	go func() {
		defer close(out)
		defer liveCancel()
		send := func(ev SessionEvent) bool {
			select {
			case out <- ev:
				return true
			case <-subCtx.Done():
				return false
			}
		}
		last := opts.FromStep - 1
		for i := range replay {
			st := replay[i]
			if !send(SessionEvent{Type: EventStepDone, SessionID: sessionID, StepIndex: st.Index, Step: &st,
				TurnID: turnIDOf(&st), TurnRole: turnRoleOf(&st), Replayed: true, At: st.CreatedAt}) {
				return
			}
			last = st.Index
		}
		for ev := range live {
			if ev.Type == EventStepDone && ev.StepIndex <= last {
				continue // already replayed
			}
			if !send(ev) {
				return
			}
		}
		if lagged() {
			mu.Lock()
			lastErr = ErrSubscriberLagged
			mu.Unlock()
		}
	}()
	return &Subscription{
		Events: out,
		err: func() error {
			mu.Lock()
			defer mu.Unlock()
			return lastErr
		},
		cancel: func() { cancel(); liveCancel() },
	}, nil
}

// emit publishes a session event. StepIndex defaults to -1 for events that
// are not about a persisted step.
func (e *Engine) emit(ctx context.Context, ev SessionEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	e.broadcaster.Publish(context.WithoutCancel(ctx), ev)
}

// turnIDOf and turnRoleOf read a step's turn linkage from its diagnostics.
func turnIDOf(st *Step) uuid.UUID {
	if s, ok := st.Diagnostics["turn_id"].(string); ok {
		if id, err := uuid.Parse(s); err == nil {
			return id
		}
	}
	return uuid.Nil
}

func turnRoleOf(st *Step) string {
	r, _ := st.Diagnostics["turn_role"].(string)
	return r
}

// RecordStep appends a step whose result was written outside loom — a human
// game master's narration, a moderator's correction — to the session history.
//
// It is persisted and checkpointed exactly like a generated step, so forks and
// StateAt see it, and pre- and post-hooks run so hooks can update state from
// it. No generator is called, no prompt is rendered, no budget is checked and
// no cost row is written. Step.Author records who wrote it.
//
// Errors are those of RunStep, minus generation and budget errors; a post-hook
// that asks for a retry is an error, since there is nothing to re-run.
func (e *Engine) RecordStep(ctx context.Context, session *Session, ext ExternalStep) (*Step, error) {
	if ext.Result == nil {
		return nil, fmt.Errorf("loom: RecordStep needs a Result")
	}
	if ext.Author == "" {
		return nil, fmt.Errorf("loom: RecordStep needs an Author")
	}
	return e.steps.run(ctx, session, StepRequest{
		AgentSlug:    ext.AgentSlug,
		Owner:        ext.Owner,
		AgentVersion: ext.AgentVersion,
		Action:       ext.Action,
		Inputs:       ext.Inputs,
		Params:       ext.Params,
		external:     ext.Result,
		author:       ext.Author,
	})
}

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
	// Sweep runs all four GC tiers ONCE and reports what it did, honouring
	// Config.BranchGC.DryRun.
	//
	// Run blocks forever on a timer and DryRun never acts, so without this
	// there is no way to collect on demand — which is what an external
	// scheduler (cron, a k8s Job, an admin endpoint) needs, and what a test
	// needs to assert retention behaviour.
	Sweep(ctx context.Context) (*SweepReport, error)
}

type gcService struct{ e *Engine }

// pendingTask is an async provider job awaiting resolution by the poller.
type pendingTask struct {
	ID        uuid.UUID
	Provider  string
	Handle    string
	Attempts  int
	SessionID uuid.UUID
	AgentID   uuid.UUID
}

// -----------------------------------------------------------------------
// JSON helpers for result serialisation
// -----------------------------------------------------------------------

// MarshalResult serialises a Result to JSON for storage.
func MarshalResult(r Result) ([]byte, error) {
	return json.Marshal(r)
}
