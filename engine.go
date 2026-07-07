// Package loom is a modality-agnostic procgen engine for building AI-driven
// interactive narratives, games, and generative platforms. It provides session
// management, versioned agents and prompts, a hook bus for pre/post processors,
// cost tracking with budget enforcement, branch/replay support, an entity
// annotator, a judge subsystem, and a first-class test harness.
package loom

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
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
	// PromptFileRoot enables file-backed prompts (PromptRef.File / PromptFromFile),
	// confined to this directory. It is empty by default, which DISABLES file
	// loading entirely: an unconfined os.ReadFile of a caller-supplied path is an
	// arbitrary-file-read primitive that exfiltrates the file's contents to the
	// LLM. When set, a PromptRef.File must resolve (after symlink evaluation) to a
	// path inside this root, or the read is rejected.
	PromptFileRoot string
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

	onTaskResolved func(context.Context, TaskResolution)

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
// -----------------------------------------------------------------------
// Stub types to satisfy forward references in this file.
// Full implementations are in their respective sub-packages.
// -----------------------------------------------------------------------

type pendingTask struct {
	ID        uuid.UUID
	Provider  string
	Handle    string
	Attempts  int
	SessionID uuid.UUID
	AgentID   uuid.UUID
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

// BudgetTarget identifies the entity a budget applies to. Only Kind ==
// TargetPlatformID is currently enforced; Create rejects other kinds.
type BudgetTarget struct {
	Kind string // currently only "platform_id" is enforced
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
// -----------------------------------------------------------------------
// JSON helpers for result serialisation
// -----------------------------------------------------------------------

// MarshalResult serialises a Result to JSON for storage.
func MarshalResult(r Result) ([]byte, error) {
	return json.Marshal(r)
}
