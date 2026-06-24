package loom

// bugrepro_test.go — runtime ("in production") reproductions of the six
// bucket-1 foundational bugs (C2, C4, C5, H3, H4, H5). White-box (package loom)
// so it can reach newBus for the H5 bus test. The non-race tests (C2, C5, H5)
// assert the DESIRED post-fix behavior, so they FAIL now (proving the bug) and
// will PASS once fixed. The race tests (C4, H3, H4) surface a DATA RACE under
// `go test -race`.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/rhaqim/loom/schema"
	_ "modernc.org/sqlite"
)

func reproEngine(t *testing.T, name string, gens map[string]Generator, poller PollerConfig) (*Engine, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+name+"_"+uuid.NewString()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{DB: db, Dialect: DialectSQLite, Generators: gens, AsyncPoller: poller})
	if err != nil {
		t.Fatal(err)
	}
	return e, db
}

type okGen struct{}

func (okGen) Modality() Modality { return ModalityText }
func (okGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return NewTextResult("ok", "stop", 1, 1), nil
}

// --- C5: streaming generator failure swallowed as a successful empty result ---

type failingStreamGen struct{}

func (failingStreamGen) Modality() Modality { return ModalityText }
func (failingStreamGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return NewTextResult("sync", "stop", 1, 1), nil
}
func (failingStreamGen) GenerateStream(context.Context, GenerateRequest) (<-chan Chunk, <-chan Result, error) {
	chunks := make(chan Chunk)
	results := make(chan Result, 1)
	go func() {
		close(chunks) // provider failed: no chunks produced
		// A corrected adapter signals a stream failure with a ResultStatusFailed
		// result; the engine must surface it rather than persist an empty turn.
		results <- NewFailedResult(ModalityText, errors.New("provider failed"))
		close(results)
	}()
	return chunks, results, nil
}

func TestReproC5_StreamingErrorSwallowed(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "c5", map[string]Generator{"g": failingStreamGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a", OnChunk: func(Chunk) {}})
	if err != nil {
		t.Logf("FIXED: streaming failure surfaced as error: %v", err)
		return
	}
	t.Errorf("BUG C5 CONFIRMED: streaming failure swallowed — RunStep err=nil; persisted result status=%q content=%q",
		step.Result.Status(), ResultText(step.Result))
}

// --- C2: non-transactional three-table step write leaves orphan rows ---

func TestReproC2_NonTransactionalOrphan(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "c2", map[string]Generator{"g": okGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Pre-occupy (session_id, step_index=0) so the FINAL steps INSERT fails UNIQUE
	// after the results row has already autocommitted.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO loom_steps (id, session_id, step_index, agent_id, result_id, created_at) VALUES (?,?,?,?,?,?)`,
		uuid.New().String(), sess.ID.String(), 0, uuid.New().String(), uuid.New().String(), time.Now()); err != nil {
		t.Fatalf("pre-insert step: %v", err)
	}

	countResults := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM loom_results`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countResults()

	_, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
	if err == nil {
		t.Fatal("expected step persistence to fail on UNIQUE(session_id, step_index)")
	}
	after := countResults()

	var orphans int
	if e2 := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM loom_results r WHERE NOT EXISTS (SELECT 1 FROM loom_steps s WHERE s.result_id = r.id)`).Scan(&orphans); e2 != nil {
		t.Fatal(e2)
	}

	if after > before && orphans > 0 {
		t.Errorf("BUG C2 CONFIRMED: non-transactional step write — results %d->%d, %d orphan result(s) committed despite step failure (%v)",
			before, after, orphans, err)
	} else {
		t.Logf("FIXED: failed step left no orphan (before=%d after=%d orphans=%d)", before, after, orphans)
	}
}

// TestReproC2_Postgres confirms C2 against a real Postgres ("in production"):
// passing an in-memory Agent whose ID is absent from loom_agents makes the final
// steps INSERT fail the agent_id FK *after* the results row has already
// committed (results has no FK to agents), leaving an orphan. Gated on LOOM_DSN.
func TestReproC2_Postgres(t *testing.T) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("set LOOM_DSN to run the Postgres production repro")
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := New(Config{DB: db, Dialect: DialectPostgres, Generators: map[string]Generator{"g": okGen{}}})
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	countResults := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM loom_results`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countResults()
	_, err = e.RunStep(ctx, sess, StepRequest{Agent: &Agent{ID: uuid.New(), Slug: "ephemeral", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}})
	if err == nil {
		t.Fatal("expected steps.agent_id FK violation")
	}
	after := countResults()
	if after > before {
		t.Errorf("BUG C2 CONFIRMED (Postgres): results %d->%d — result row committed though the step FK-failed (%v)", before, after, err)
	} else {
		t.Logf("FIXED (Postgres): no orphan result after failed step (before=%d after=%d)", before, after)
	}
}

// --- C4: async poller reads the generators map without the lock ---

func TestReproC4_PollerGeneratorsMapRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, db := reproEngine(t, "c4", map[string]Generator{"seed": okGen{}}, PollerConfig{Interval: time.Millisecond, Workers: 2})

	now := time.Now()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO loom_tasks (id, provider, handle, status, created_at, updated_at) VALUES (?,?,?,'pending',?,?)`,
		uuid.New().String(), "seed", "h", now, now); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	e.StartPoller(ctx)

	done := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
				e.RegisterGenerator(fmt.Sprintf("g%d", i), okGen{})
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	close(done)
	cancel()
	time.Sleep(20 * time.Millisecond)
	t.Log("C4: poller + concurrent RegisterGenerator ran; under -race a DATA RACE (read engine.go:871 vs write engine.go:163) confirms the bug")
}

// --- H3: pre-hook mutation of shared session.State.Vars races followers ---

func TestReproH3_PreHookVarsRace(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "h3", map[string]Generator{"g": okGen{}}, PollerConfig{})

	// A barrier releases both FOLLOWER pre-hooks at the same instant so their
	// writes to the shared session.State.Vars map genuinely overlap (the race is
	// real but dynamic detection needs the accesses to actually coincide).
	var arrived sync.WaitGroup
	arrived.Add(2)
	gate := make(chan struct{})
	go func() { arrived.Wait(); close(gate) }()
	e.Hooks().RegisterPre("vars-writer", func(_ context.Context, req *StepRequest) error {
		if req.AgentSlug != "lead" {
			arrived.Done()
			<-gate
		}
		req.Session.State.Vars["k"] = req.AgentSlug // concurrent map write across followers
		return nil
	})

	for _, slug := range []string{"lead", "f1", "f2"} {
		if err := e.Agents().Create(ctx, &Agent{Slug: slug, Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
			t.Fatal(err)
		}
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText, Vars: map[string]any{"k": "init"}}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	if _, err := e.RunTurn(ctx, sess, TurnRequest{Flow: Flow{
		Slug: "t", Lead: FlowAgent{AgentSlug: "lead"},
		Followers: []FlowAgent{{AgentSlug: "f1"}, {AgentSlug: "f2"}},
	}}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	t.Log("H3: two followers wrote shared State.Vars simultaneously; under -race a DATA RACE confirms the bug")
}

// --- H4: concurrent read/write on the shared inputs map in RunTurn ---

func TestReproH4_RunTurnInputsRace(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "h4", map[string]Generator{"g": okGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "lead", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// A large Inputs map widens the unguarded cloneInputs read window
	// (flow.go:166) so a sibling follower's OutputKey write (flow.go:188, under a
	// different lock) overlaps it. Many followers maximize the overlap.
	bigInputs := make(map[string]any, 200_000)
	for i := 0; i < 200_000; i++ {
		bigInputs[fmt.Sprintf("k%d", i)] = i
	}
	followers := make([]FlowAgent, 0, 12)
	for i := 0; i < 12; i++ {
		slug := fmt.Sprintf("f%d", i)
		if err := e.Agents().Create(ctx, &Agent{Slug: slug, Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
			t.Fatal(err)
		}
		followers = append(followers, FlowAgent{AgentSlug: slug, OutputKey: fmt.Sprintf("K%d", i)})
	}

	if _, err := e.RunTurn(ctx, sess, TurnRequest{
		Inputs: bigInputs,
		Flow: Flow{
			Slug: "t", Lead: FlowAgent{AgentSlug: "lead"},
			Followers: followers,
		},
	}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	t.Log("H4: followers with OutputKeys ran concurrently over a large inputs map; under -race a DATA RACE confirms the bug")
}

// --- H5: bus dispatcher blocking sends wedge the turn on a non-draining sub ---

func TestReproH5_BusDeadlock(t *testing.T) {
	b := newBus()
	_ = b.Subscribe("t") // subscriber that never drains its channel
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			b.Publish("pub", "t", i)
		}
		close(done)
	}()
	select {
	case <-done:
		t.Log("FIXED: publishing did not wedge despite a non-draining subscriber")
	case <-time.After(2 * time.Second):
		t.Errorf("BUG H5 CONFIRMED: bus deadlock — non-draining subscriber wedged the dispatcher; publisher blocked (goroutine leaked)")
	}
}
