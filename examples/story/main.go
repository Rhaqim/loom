// story is an example Loom application that runs a simple interactive
// narrative pipeline using PostgreSQL for persistence.
//
// It demonstrates:
//   - Engine setup with a stub echo generator (no real API key needed)
//   - Schema migration on startup
//   - Seeding a versioned agent + prompt pair
//   - Running a three-step session (setup → conflict → resolution)
//   - Forking a session at step 1 to explore an alternate branch
//   - Printing cost usage for the session
//
// Run it after `make up && make migrate`:
//
//	LOOM_DSN="postgres://loom:loom@localhost:5432/loom?sslmode=disable" go run ./examples/story
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/lib/pq"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/generator/echo"
	"github.com/rhaqim/loom/schema"
)

func main() {
	ctx := context.Background()

	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		dsn = "postgres://loom:loom@localhost:5432/loom?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Wait for DB to be ready (up to 15s).
	for i := range 15 {
		if err = db.PingContext(ctx); err == nil {
			break
		}
		log.Printf("waiting for db... (%d/15)", i+1)
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Fatalf("db not ready: %v", err)
	}

	// Apply schema (idempotent).
	loader := schema.NewLoader(schema.DialectPostgres)
	if err := loader.Apply(ctx, db); err != nil {
		log.Fatalf("schema: %v", err)
	}
	fmt.Println("[story] schema applied")

	// Build the engine with the echo (stub) generator.
	e, err := loom.New(loom.Config{
		DB:      db,
		Dialect: loom.DialectPostgres,
		Generators: map[string]loom.Generator{
			"echo": echo.New("[narrator]"),
		},
		Logger: &stdLogger{},
	})
	if err != nil {
		log.Fatalf("engine: %v", err)
	}

	// Seed a system prompt and user template (idempotent via slug+version).
	sysPrmt := &loom.Prompt{
		Slug:     "narrator-system",
		Version:  1,
		Kind:     loom.PromptKindSystem,
		Category: "story",
		Body:     "You are a creative narrator for an interactive fantasy story.",
	}
	userTmpl := &loom.Prompt{
		Slug:     "narrator-user",
		Version:  1,
		Kind:     loom.PromptKindUserTemplate,
		Category: "story",
		Body:     `Continue the story. The player chose: {{.Action.Payload.text}}. Current scene: {{index .Vars "scene"}}.`,
	}

	seedPrompt(ctx, e, sysPrmt)
	seedPrompt(ctx, e, userTmpl)

	// Resolve prompt IDs for the agent.
	sys, _ := e.Prompts().Get(ctx, "narrator-system", 1)
	ut, _ := e.Prompts().Get(ctx, "narrator-user", 1)

	agent := &loom.Agent{
		Slug:           "narrator",
		Version:        1,
		Category:       "story",
		Modal:          loom.ModalityText,
		GeneratorSlug:  "echo",
		SystemPromptID: sys.ID,
		UserTemplateID: ut.ID,
	}
	seedAgent(ctx, e, agent)

	// -----------------------------------------------------------------------
	// Main session: three-step narrative
	// -----------------------------------------------------------------------
	sess := &loom.Session{
		PlatformID: "player-001",
		Tags:       []string{"example"},
		State: loom.State{
			Modality: loom.ModalityText,
			Vars: map[string]any{
				"scene": "A dark forest path. Two roads diverge ahead.",
			},
		},
	}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		log.Fatalf("create session: %v", err)
	}
	fmt.Printf("\n[story] session %s started\n", sess.ID)

	actions := []string{
		"Take the left road into the forest",
		"Light a torch and look for tracks",
		"Follow the tracks deeper into the woods",
	}

	var forkAtStep int = 1 // fork after step index 1
	var forkSessionID = sess.ID

	for i, payload := range actions {
		step, err := e.RunStep(ctx, sess, loom.StepRequest{
			AgentSlug: "narrator",
			Action: &loom.Action{
				Kind:    loom.ActionFreeText,
				Payload: map[string]any{"text": payload},
			},
		})
		if err != nil {
			log.Fatalf("step %d: %v", i, err)
		}
		tr := step.Result.(*loom.TextResult)
		fmt.Printf("\n  step[%d] action=%q\n  → %s\n", i, payload, tr.Content)

		// Capture the session ID just before the fork point.
		if i == forkAtStep {
			forkSessionID = sess.ID
		}
	}

	// -----------------------------------------------------------------------
	// Fork: explore an alternate branch from step 1
	// -----------------------------------------------------------------------
	fmt.Printf("\n[story] forking session at step %d...\n", forkAtStep)
	branch, err := e.Sessions().Fork(ctx, forkSessionID, forkAtStep)
	if err != nil {
		log.Fatalf("fork: %v", err)
	}
	fmt.Printf("[story] branch session %s created\n", branch.ID)

	altStep, err := e.RunStep(ctx, branch, loom.StepRequest{
		AgentSlug: "narrator",
		Action: &loom.Action{
			Kind:    loom.ActionFreeText,
			Payload: map[string]any{"text": "Take the RIGHT road toward the mountains"},
		},
	})
	if err != nil {
		log.Fatalf("alt step: %v", err)
	}
	fmt.Printf("\n  [branch] action=%q\n  → %s\n",
		"Take the RIGHT road toward the mountains",
		altStep.Result.(*loom.TextResult).Content)

	// -----------------------------------------------------------------------
	// Cost summary
	// -----------------------------------------------------------------------
	fmt.Println("\n[story] cost summary (main session):")
	usage, err := e.Cost().SessionUsage(ctx, sess.ID)
	if err != nil {
		log.Printf("  cost: %v", err)
	} else {
		fmt.Printf("  steps=%d  tokens=%d  usd=%.6f\n",
			usage.StepCount, usage.TotalTokens, usage.TotalUSD)
	}

	fmt.Println("\n[story] done.")
}

// seedPrompt inserts a prompt if it doesn't already exist.
func seedPrompt(ctx context.Context, e *loom.Engine, p *loom.Prompt) {
	existing, _ := e.Prompts().Get(ctx, p.Slug, p.Version)
	if existing != nil {
		return
	}
	if err := e.Prompts().Create(ctx, p); err != nil {
		log.Printf("seed prompt %q: %v", p.Slug, err)
	}
}

// seedAgent inserts an agent if it doesn't already exist.
func seedAgent(ctx context.Context, e *loom.Engine, a *loom.Agent) {
	existing, _ := e.Agents().Get(ctx, a.Slug, a.Version)
	if existing != nil {
		return
	}
	if err := e.Agents().Create(ctx, a); err != nil {
		log.Printf("seed agent %q: %v", a.Slug, err)
	}
}

// stdLogger is a simple Logger implementation that writes to stdout.
type stdLogger struct{}

func (l *stdLogger) Info(msg string, kv ...any) {
	args := fmtKV(kv)
	if args != "" {
		fmt.Printf("[loom] %s  %s\n", msg, args)
	}
}
func (l *stdLogger) Error(msg string, kv ...any) {
	fmt.Printf("[loom:error] %s  %s\n", msg, fmtKV(kv))
}
func fmtKV(kv []any) string {
	s := ""
	for i := 0; i+1 < len(kv); i += 2 {
		s += fmt.Sprintf("%v=%v ", kv[i], kv[i+1])
	}
	return s
}
