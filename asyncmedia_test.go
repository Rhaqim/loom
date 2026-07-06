package loom

// asyncmedia_test.go — end-to-end coverage for the async media (fire-and-poll)
// pipeline. Before the fix a pending result never inserted a loom_tasks row (so
// the poller had nothing to find, and on Postgres the step insert FK-failed), and
// the poller discarded the resolved Result so the result stayed pending forever.
// These tests drive a real RunStep with a stub async generator, then run the
// poller and assert the resolved asset lands in loom_results.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/loom/schema"
)

// asyncImageGen is a stub async image generator. Generate returns a pending
// result with a task handle; Poll resolves it to a ready ImageResult after an
// optional run of transient errors.
type asyncImageGen struct {
	failTimes int // number of leading Poll calls that return a transient error
	calls     int
}

func (*asyncImageGen) Modality() Modality { return ModalityImage }
func (*asyncImageGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return NewPendingResult(ModalityImage, &TaskHandle{ID: uuid.New(), Provider: "asyncimg", Handle: "job-1"}), nil
}
func (g *asyncImageGen) Poll(ctx context.Context, h TaskHandle) (Result, error) {
	g.calls++
	if g.calls <= g.failTimes {
		return nil, errors.New("transient provider error")
	}
	return NewImageResult("https://cdn.example/"+h.Handle+".png", "a prompt", 512, 512), nil
}

// failedResultGen always resolves a job to a terminal failed result.
type failedResultGen struct{}

func (failedResultGen) Modality() Modality { return ModalityImage }
func (failedResultGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return NewPendingResult(ModalityImage, &TaskHandle{ID: uuid.New(), Provider: "failres", Handle: "job-x"}), nil
}
func (failedResultGen) Poll(ctx context.Context, h TaskHandle) (Result, error) {
	return NewFailedResult(ModalityImage, errors.New("content policy violation")), nil
}

// runPending runs a real step through the engine with the given async generator
// and returns the pending task handle the engine persisted.
func runPending(t *testing.T, ctx context.Context, e *Engine, provider string) *TaskHandle {
	t.Helper()
	// Persist the agent first (an inline req.Agent is not auto-persisted, so its
	// FK from the step would fail on Postgres), then run the step by slug.
	agent := &Agent{ID: uuid.New(), Slug: "async-" + uuid.NewString()[:8], Version: 1, Modal: ModalityImage, GeneratorSlug: provider}
	if err := e.Agents().Create(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	sess := &Session{PlatformID: "async-test", State: State{Modality: ModalityImage}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: agent.Slug, AgentVersion: agent.Version})
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	th := step.Result.TaskHandle()
	if th == nil {
		t.Fatalf("want a pending result with a task handle, got status %q", step.Result.Status())
	}
	return th
}

func taskStatus(t *testing.T, db *sql.DB, prefix string, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := db.QueryRow(fmt.Sprintf(`SELECT status FROM %stasks WHERE id=$1`, prefix), id).Scan(&s); err != nil {
		t.Fatalf("query task status: %v", err)
	}
	return s
}

func resultByTask(t *testing.T, db *sql.DB, prefix string, taskID uuid.UUID) (status, payload string) {
	t.Helper()
	if err := db.QueryRow(fmt.Sprintf(`SELECT status, payload FROM %sresults WHERE task_id=$1`, prefix), taskID).
		Scan(&status, &payload); err != nil {
		t.Fatalf("query result by task: %v", err)
	}
	return status, payload
}

func TestAsyncMedia_PendingResolvesEndToEnd(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "asyncmedia_ok", map[string]Generator{"asyncimg": &asyncImageGen{}}, PollerConfig{Interval: time.Minute, Workers: 1})

	th := runPending(t, ctx, e, "asyncimg")

	// The task row exists and both task and result start pending.
	if got := taskStatus(t, db, e.prefix, th.ID); got != "pending" {
		t.Fatalf("task status before poll = %q, want pending", got)
	}
	if status, _ := resultByTask(t, db, e.prefix, th.ID); status != "pending" {
		t.Fatalf("result status before poll = %q, want pending", status)
	}

	newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1}).pollPending(ctx)

	// The job resolved: task ready, result ready and carrying the image URL.
	if got := taskStatus(t, db, e.prefix, th.ID); got != "ready" {
		t.Fatalf("task status after poll = %q, want ready", got)
	}
	status, payload := resultByTask(t, db, e.prefix, th.ID)
	if status != "ready" {
		t.Fatalf("result status after poll = %q, want ready", status)
	}
	if !strings.Contains(payload, "job-1.png") {
		t.Fatalf("resolved payload missing image url: %s", payload)
	}
}

func TestAsyncMedia_TerminalFailureAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	const maxAttempts = 3
	// Always errors on Poll, so it should fail terminally after maxAttempts.
	e, db := reproEngine(t, "asyncmedia_fail", map[string]Generator{"asyncimg": &asyncImageGen{failTimes: 100}}, PollerConfig{Interval: time.Minute, Workers: 1})
	th := runPending(t, ctx, e, "asyncimg")

	p := newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1, MaxAttempts: maxAttempts})
	for range maxAttempts {
		p.pollPending(ctx)
	}

	if got := taskStatus(t, db, e.prefix, th.ID); got != "failed" {
		t.Fatalf("task status = %q, want failed after %d attempts", got, maxAttempts)
	}
	if status, _ := resultByTask(t, db, e.prefix, th.ID); status != "failed" {
		t.Fatalf("result status = %q, want failed", status)
	}
}

// pendingForeverGen never resolves: Poll keeps returning a pending result.
type pendingForeverGen struct{}

func (pendingForeverGen) Modality() Modality { return ModalityImage }
func (pendingForeverGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return NewPendingResult(ModalityImage, &TaskHandle{ID: uuid.New(), Provider: "pendingforever", Handle: "job-stuck"}), nil
}
func (pendingForeverGen) Poll(ctx context.Context, h TaskHandle) (Result, error) {
	return NewPendingResult(ModalityImage, &TaskHandle{ID: h.ID, Provider: h.Provider, Handle: h.Handle}), nil
}

func TestAsyncMedia_PerpetuallyPendingIsCapped(t *testing.T) {
	ctx := context.Background()
	const maxAttempts = 3
	e, db := reproEngine(t, "asyncmedia_stuck", map[string]Generator{"pendingforever": pendingForeverGen{}}, PollerConfig{Interval: time.Minute, Workers: 1})
	th := runPending(t, ctx, e, "pendingforever")

	p := newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1, MaxAttempts: maxAttempts})
	for range maxAttempts {
		p.pollPending(ctx)
	}

	if got := taskStatus(t, db, e.prefix, th.ID); got != "failed" {
		t.Fatalf("task status = %q, want failed (capped) after %d pending polls", got, maxAttempts)
	}
}

func TestAsyncMedia_FailedResultMarksFailed(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "asyncmedia_failres", map[string]Generator{"failres": failedResultGen{}}, PollerConfig{Interval: time.Minute, Workers: 1})
	th := runPending(t, ctx, e, "failres")

	newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1}).pollPending(ctx)

	if got := taskStatus(t, db, e.prefix, th.ID); got != "failed" {
		t.Fatalf("task status = %q, want failed", got)
	}
	if status, _ := resultByTask(t, db, e.prefix, th.ID); status != "failed" {
		t.Fatalf("result status = %q, want failed", status)
	}
}

// TestAsyncMedia_PostgresFKOrdering is the dialect-specific repro: on Postgres,
// loom_results.task_id has a real foreign key to loom_tasks(id), so persisting a
// pending result FK-violated before the fix (no task row was inserted). It also
// proves the full resolve path on Postgres. Gated on LOOM_DSN.
func TestAsyncMedia_PostgresFKOrdering(t *testing.T) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("set LOOM_DSN to run the Postgres async-media repro")
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := schema.NewLoader(schema.DialectPostgres).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{DB: db, Dialect: DialectPostgres, Generators: map[string]Generator{"asyncimg": &asyncImageGen{}}})
	if err != nil {
		t.Fatal(err)
	}

	// This step's result is pending; before the fix its insert FK-violated
	// because no loom_tasks row existed for results.task_id.
	th := runPending(t, ctx, e, "asyncimg")
	if got := taskStatus(t, db, e.prefix, th.ID); got != "pending" {
		t.Fatalf("task status before poll = %q, want pending", got)
	}

	newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1}).pollPending(ctx)

	if got := taskStatus(t, db, e.prefix, th.ID); got != "ready" {
		t.Fatalf("task status after poll = %q, want ready", got)
	}
	status, payload := resultByTask(t, db, e.prefix, th.ID)
	if status != "ready" {
		t.Fatalf("result status after poll = %q, want ready", status)
	}
	if !strings.Contains(payload, "job-1.png") {
		t.Fatalf("resolved payload missing image url: %s", payload)
	}
}
