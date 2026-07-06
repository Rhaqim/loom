// Package enginetest contains integration tests for the Loom engine.
// Tests are skipped when the LOOM_DSN environment variable is not set.
package enginetest

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/generator/echo"
	"github.com/rhaqim/loom/schema"
)

// openTestDB opens a real Postgres connection and applies the schema.
// The test is skipped if LOOM_DSN is unset.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("LOOM_DSN not set — skipping integration test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(5)
	db.SetConnMaxLifetime(time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 10; i++ {
		if err = db.PingContext(ctx); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("db ping: %v", err)
	}

	loader := schema.NewLoader(schema.DialectPostgres)
	if err := loader.Apply(ctx, db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newTestEngine creates a Loom engine backed by a real Postgres DB.
func newTestEngine(t *testing.T) *loom.Engine {
	t.Helper()
	db := openTestDB(t)
	e, err := loom.New(loom.Config{
		DB:      db,
		Dialect: loom.DialectPostgres,
		Generators: map[string]loom.Generator{
			"echo": echo.New("[test]"),
		},
	})
	if err != nil {
		t.Fatalf("loom.New: %v", err)
	}
	return e
}

// seedTestAgent creates a prompt pair + agent in the DB, returning the agent slug.
func seedTestAgent(t *testing.T, e *loom.Engine, slug string) string {
	t.Helper()
	ctx := context.Background()

	// Use unique slugs per test to avoid collisions between parallel runs.
	sysPrmt := &loom.Prompt{
		Slug:     slug + "-sys",
		Version:  1,
		Kind:     loom.PromptKindSystem,
		Category: "test",
		Body:     "You are a test assistant.",
	}
	userTmpl := &loom.Prompt{
		Slug:     slug + "-user",
		Version:  1,
		Kind:     loom.PromptKindUserTemplate,
		Category: "test",
		Body:     "User says: {{.Action.Payload.text}}",
	}

	// Ignore duplicate-slug errors — test may re-run.
	_ = e.Prompts().Create(ctx, sysPrmt)
	_ = e.Prompts().Create(ctx, userTmpl)

	sys, err := e.Prompts().Get(ctx, slug+"-sys", 1)
	if err != nil {
		t.Fatalf("get system prompt: %v", err)
	}
	ut, err := e.Prompts().Get(ctx, slug+"-user", 1)
	if err != nil {
		t.Fatalf("get user template: %v", err)
	}

	agent := &loom.Agent{
		Slug:           slug,
		Version:        1,
		Category:       "test",
		Modal:          loom.ModalityText,
		GeneratorSlug:  "echo",
		SystemPromptID: sys.ID,
		UserTemplateID: ut.ID,
	}
	_ = e.Agents().Create(ctx, agent) // idempotent on slug+version collision
	return slug
}

// -----------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------

func TestEngine_CreateAndGetSession(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	sess := &loom.Session{
		PlatformID: "test-user",
		Tags:       []string{"test:true"},
		State: loom.State{
			Modality: loom.ModalityText,
			Vars:     map[string]any{"scene": "start"},
		},
	}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ID.String() == "" {
		t.Fatal("expected non-empty session ID")
	}

	got, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PlatformID != sess.PlatformID {
		t.Errorf("PlatformID: got %q want %q", got.PlatformID, sess.PlatformID)
	}
}

func TestEngine_RunStep_TextResult(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	agentSlug := seedTestAgent(t, e, "eng-rsstep-"+randomSuffix())

	sess := &loom.Session{
		PlatformID: "test-user",
		Tags:       []string{"test:true"},
		State: loom.State{
			Modality: loom.ModalityText,
			Vars:     map[string]any{},
		},
	}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	step, err := e.RunStep(ctx, sess, loom.StepRequest{
		AgentSlug: agentSlug,
		Action: &loom.Action{
			Kind:    loom.ActionFreeText,
			Payload: map[string]any{"text": "hello world"},
		},
	})
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if step.Result == nil {
		t.Fatal("expected non-nil result")
	}
	if step.Result.Status() != loom.ResultStatusReady {
		t.Errorf("status: got %q want ready", step.Result.Status())
	}
	tr, ok := step.Result.(*loom.TextResult)
	if !ok {
		t.Fatalf("expected *TextResult, got %T", step.Result)
	}
	if tr.Content == "" {
		t.Error("expected non-empty content")
	}
}

func TestEngine_MultiStep_SessionHistory(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	agentSlug := seedTestAgent(t, e, "eng-ms-"+randomSuffix())

	sess := &loom.Session{
		PlatformID: "test-user",
		Tags:       []string{"test:true"},
		State:      loom.State{Modality: loom.ModalityText, Vars: map[string]any{}},
	}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := range 3 {
		_, err := e.RunStep(ctx, sess, loom.StepRequest{
			AgentSlug: agentSlug,
			Action: &loom.Action{
				Kind:    loom.ActionFreeText,
				Payload: map[string]any{"text": "step " + string(rune('A'+i))},
			},
		})
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}

	if len(sess.History) != 3 {
		t.Errorf("in-memory history: got %d want 3", len(sess.History))
	}

	// Fetch from DB and check persisted history length.
	loaded, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(loaded.History) != 3 {
		t.Errorf("db history: got %d want 3", len(loaded.History))
	}
}

func TestEngine_ForkSession(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	agentSlug := seedTestAgent(t, e, "eng-fork-"+randomSuffix())

	sess := &loom.Session{
		PlatformID: "test-user",
		Tags:       []string{"test:true"},
		State:      loom.State{Modality: loom.ModalityText, Vars: map[string]any{}},
	}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Run two steps on the main branch.
	for i := range 2 {
		_, err := e.RunStep(ctx, sess, loom.StepRequest{
			AgentSlug: agentSlug,
			Action: &loom.Action{
				Kind:    loom.ActionFreeText,
				Payload: map[string]any{"text": "main " + string(rune('0'+i))},
			},
		})
		if err != nil {
			t.Fatalf("main step %d: %v", i, err)
		}
	}

	// Fork after step 0.
	branch, err := e.Sessions().Fork(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if branch.ID == sess.ID {
		t.Fatal("branch should have different ID")
	}
	if *branch.ParentSessionID != sess.ID {
		t.Error("expected parent session ID to match")
	}

	// Run a divergent step on the branch.
	_, err = e.RunStep(ctx, branch, loom.StepRequest{
		AgentSlug: agentSlug,
		Action: &loom.Action{
			Kind:    loom.ActionFreeText,
			Payload: map[string]any{"text": "branch diverges"},
		},
	})
	if err != nil {
		t.Fatalf("branch step: %v", err)
	}

	// Verify branch tree.
	tree, err := e.Sessions().BranchTree(ctx, sess.ID)
	if err != nil {
		t.Fatalf("BranchTree: %v", err)
	}
	if tree.Session.ID != sess.ID {
		t.Error("tree root should be the original session")
	}
	if len(tree.Children) == 0 {
		t.Error("expected at least one child branch in tree")
	}
}

func TestEngine_CostRecording(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	agentSlug := seedTestAgent(t, e, "eng-cost-"+randomSuffix())

	sess := &loom.Session{
		PlatformID: "cost-test-user",
		Tags:       []string{"test:true"},
		State:      loom.State{Modality: loom.ModalityText, Vars: map[string]any{}},
	}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := e.RunStep(ctx, sess, loom.StepRequest{
		AgentSlug: agentSlug,
		Action: &loom.Action{
			Kind:    loom.ActionFreeText,
			Payload: map[string]any{"text": "cost tracking test"},
		},
	})
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}

	// Cost records are written in a background goroutine; give it a moment.
	time.Sleep(200 * time.Millisecond)

	usage, err := e.Cost().SessionUsage(ctx, sess.ID)
	if err != nil {
		t.Fatalf("SessionUsage: %v", err)
	}
	if usage.StepCount != 1 {
		t.Errorf("StepCount: got %d want 1", usage.StepCount)
	}
}

func TestEngine_PreHook_Skip(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	agentSlug := seedTestAgent(t, e, "eng-prehook-"+randomSuffix())

	// Register a pre-hook that always skips.
	e.Hooks().RegisterPre("always-skip", func(_ context.Context, _ *loom.StepRequest) error {
		return loom.ErrSkip
	})

	sess := &loom.Session{
		PlatformID: "test-user",
		Tags:       []string{"test:true"},
		State:      loom.State{Modality: loom.ModalityText, Vars: map[string]any{}},
	}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := e.RunStep(ctx, sess, loom.StepRequest{
		AgentSlug: agentSlug,
		Action: &loom.Action{
			Kind:    loom.ActionFreeText,
			Payload: map[string]any{"text": "should be skipped"},
		},
	})
	if !loom.IsSkip(err) {
		t.Errorf("expected skip error, got: %v", err)
	}
}

func TestEngine_Prompts_CRUD(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	slug := "test-prompt-" + randomSuffix()
	p := &loom.Prompt{
		Slug:     slug,
		Version:  1,
		Kind:     loom.PromptKindSystem,
		Category: "test",
		Body:     "A test prompt body.",
		Notes:    "created in TestEngine_Prompts_CRUD",
	}
	if err := e.Prompts().Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := e.Prompts().Get(ctx, slug, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != p.Body {
		t.Errorf("Body: got %q want %q", got.Body, p.Body)
	}

	list, err := e.Prompts().List(ctx, loom.PromptKindSystem, "test")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, pr := range list {
		if pr.Slug == slug {
			found = true
			break
		}
	}
	if !found {
		t.Error("prompt not found in list")
	}
}

func TestEngine_Agents_CRUD(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	slug := "test-agent-" + randomSuffix()
	a := &loom.Agent{
		Slug:          slug,
		Version:       1,
		Category:      "test",
		Modal:         loom.ModalityText,
		GeneratorSlug: "echo",
	}
	if err := e.Agents().Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := e.Agents().Get(ctx, slug, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Slug != slug {
		t.Errorf("Slug: got %q want %q", got.Slug, slug)
	}

	latest, err := e.Agents().Latest(ctx, slug)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.Version != 1 {
		t.Errorf("Latest.Version: got %d want 1", latest.Version)
	}
}

func TestEngine_Budget_CreateAndList(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	b := &loom.Budget{
		Name: "test-budget-" + randomSuffix(),
		Target: loom.BudgetTarget{
			Kind: loom.TargetPlatformID,
			Key:  "budget-test-user",
		},
		Window:   loom.BudgetWindowDay,
		Limit:    loom.BudgetLimit{USD: 1.0, Steps: 100},
		OnExceed: loom.BudgetBlock,
		Active:   true,
	}
	if err := e.Budgets().Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := e.Budgets().Get(ctx, b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Limit.USD != 1.0 {
		t.Errorf("Limit.USD: got %v want 1.0", got.Limit.USD)
	}

	list, err := e.Budgets().List(ctx, loom.TargetPlatformID, "budget-test-user")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) == 0 {
		t.Error("expected at least one budget")
	}

	if err := e.Budgets().Delete(ctx, b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestHarness_RunPlan(t *testing.T) {
	// Import the harness package inline to test the full pipeline.
	e := newTestEngine(t)
	ctx := context.Background()

	agentSlug := seedTestAgent(t, e, "harness-"+randomSuffix())

	plan := buildHarnessPlan(agentSlug)
	report, err := runHarness(ctx, e, plan)
	if err != nil {
		t.Fatalf("harness.Run: %v", err)
	}
	if !report.Passed() {
		for _, v := range report.Variants {
			for _, s := range v.Steps {
				for _, f := range s.Failures {
					t.Errorf("variant %d step %d: %s", v.Index, s.StepIndex, f)
				}
			}
		}
	}
}
