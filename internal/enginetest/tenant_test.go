package enginetest

// tenant_test.go — owner (tenant) isolation against a REAL Postgres database.
//
// The equivalent unit tests run on SQLite. These re-assert the same contract on
// Postgres because that is what production deployments use, and the two
// dialects differ in exactly the places this feature touches: placeholder
// binding, the unique-key definition, and NULL/empty-string handling on the
// owner column.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/generator/echo"
)

// slug keeps each run independent — the Postgres database is shared and
// persists across tests.
func tenantSlug(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

func TestTenantIsolation_Postgres(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)

	slug := tenantSlug("narrator")
	for _, owner := range []string{"tenant-a", "tenant-b"} {
		if err := e.Agents().Create(ctx, &loom.Agent{
			Slug: slug, Version: 1, Owner: owner,
			Modal: loom.ModalityText, GeneratorSlug: "echo",
			Category: owner + "-cat",
		}); err != nil {
			// Two owners sharing a slug is only possible with the widened
			// unique key, so a failure here means the key did not widen.
			t.Fatalf("create agent for %s (widened unique key missing?): %v", owner, err)
		}
	}

	// Each tenant sees its own record.
	for _, owner := range []string{"tenant-a", "tenant-b"} {
		got, err := e.Agents().Get(ctx, owner, slug, 1)
		if err != nil {
			t.Fatalf("Get as %s: %v", owner, err)
		}
		if got.Category != owner+"-cat" {
			t.Fatalf("Get as %s returned %q — cross-tenant read", owner, got.Category)
		}
	}

	// A third tenant sees nothing, and does not fall through to the global scope.
	if _, err := e.Agents().Get(ctx, "tenant-c", slug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("Get as tenant-c: err = %v, want ErrNotFound", err)
	}
	if _, err := e.Agents().Get(ctx, "", slug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("Get as global scope: err = %v, want ErrNotFound", err)
	}

	// Latest resolves within the owner, not across owners.
	latest, err := e.Agents().Latest(ctx, "tenant-a", slug)
	if err != nil {
		t.Fatalf("Latest as tenant-a: %v", err)
	}
	if latest.Owner != "tenant-a" {
		t.Fatalf("Latest as tenant-a returned owner %q", latest.Owner)
	}

	// List must not leak either.
	listed, err := e.Agents().List(ctx, "tenant-a", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range listed {
		if a.Owner != "tenant-a" {
			t.Fatalf("List as tenant-a returned an agent owned by %q", a.Owner)
		}
	}
}

// Step execution resolves agents by slug, so isolation has to hold there too —
// registry scoping alone does not cover it.
func TestTenantIsolation_StepExecution_Postgres(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)

	slug := tenantSlug("author")
	if err := e.Agents().Create(ctx, &loom.Agent{
		Slug: slug, Version: 1, Owner: "tenant-a",
		Modal: loom.ModalityText, GeneratorSlug: "echo",
	}); err != nil {
		t.Fatal(err)
	}

	sess := &loom.Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	if _, err := e.RunStep(ctx, sess, loom.StepRequest{Owner: "tenant-a", AgentSlug: slug}); err != nil {
		t.Fatalf("tenant-a running its own agent: %v", err)
	}
	if _, err := e.RunStep(ctx, sess, loom.StepRequest{Owner: "tenant-b", AgentSlug: slug}); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("tenant-b executing tenant-a's agent: err = %v, want ErrNotFound", err)
	}
	if _, err := e.RunStep(ctx, sess, loom.StepRequest{AgentSlug: slug}); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("global scope executing an owned agent: err = %v, want ErrNotFound", err)
	}
}

// Prompts carry a 4-column key (owner, slug, version, kind); confirm the widened
// key and the scoped read both behave on Postgres.
func TestTenantIsolation_Prompts_Postgres(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)

	slug := tenantSlug("sys")
	for _, owner := range []string{"tenant-a", "tenant-b"} {
		if err := e.Prompts().Create(ctx, &loom.Prompt{
			Slug: slug, Version: 1, Owner: owner,
			Kind: loom.PromptKindSystem, Body: owner + " body",
		}); err != nil {
			t.Fatalf("create prompt for %s: %v", owner, err)
		}
	}
	got, err := e.Prompts().Get(ctx, "tenant-b", slug, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "tenant-b body" {
		t.Fatalf("Get as tenant-b returned %q — cross-tenant read", got.Body)
	}
	if _, err := e.Prompts().Get(ctx, "tenant-c", slug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("Get as tenant-c: err = %v, want ErrNotFound", err)
	}
}

// TestCostSavepointIsolation_Postgres is the dialect that matters for the
// SAVEPOINT: on Postgres any error inside a transaction poisons it (25P02)
// until the savepoint is unwound, so without the ROLLBACK TO a failed cost
// insert would abort the whole step. SQLite does not behave that way, so the
// unit test cannot prove this — only a real Postgres run can.
func TestCostSavepointIsolation_Postgres(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	e, err := loom.New(loom.Config{
		DB: db, Dialect: loom.DialectPostgres,
		Generators: map[string]loom.Generator{"echo": echo.New("[test]")},
	})
	if err != nil {
		t.Fatal(err)
	}

	slug := tenantSlug("cost-sp")
	if err := e.Agents().Create(ctx, &loom.Agent{
		Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: "echo",
	}); err != nil {
		t.Fatal(err)
	}
	sess := &loom.Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Force the cost insert to fail on its own, without disturbing any other
	// write in the step transaction: a CHECK that no cost row can satisfy.
	// Dropping the table would break every other test sharing this database.
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE loom_cost_records ADD CONSTRAINT loom_cost_reject CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.ExecContext(context.Background(),
			`ALTER TABLE loom_cost_records DROP CONSTRAINT IF EXISTS loom_cost_reject`)
	})

	step, err := e.RunStep(ctx, sess, loom.StepRequest{AgentSlug: slug})
	if err != nil {
		t.Fatalf("a cost-write failure discarded the step on Postgres: %v", err)
	}

	// The step and its checkpoint must be durable despite the failed cost row.
	got, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 {
		t.Fatalf("history = %d steps, want 1 — the transaction was aborted", len(got.History))
	}
	if _, found, err := e.Sessions().StateAt(ctx, sess.ID, step.Index); err != nil || !found {
		t.Fatalf("checkpoint missing: found=%v err=%v", found, err)
	}

	// With the constraint gone, cost recording works again and is durable
	// immediately — proving the savepoint left the connection usable.
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE loom_cost_records DROP CONSTRAINT loom_cost_reject`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sess, loom.StepRequest{AgentSlug: slug}); err != nil {
		t.Fatal(err)
	}
	usage, err := e.Cost().ByStep(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 {
		t.Fatalf("ByStep = %d rows, want 1 (the second step only)", len(usage))
	}
}

// TestLineage_Postgres exercises the recursive-CTE lineage walks on Postgres.
// BranchTree and Ancestry are the only queries in loom using WITH RECURSIVE,
// and CTE behaviour is where the two dialects most plausibly diverge — so the
// SQLite unit tests are not sufficient evidence for a Postgres deployment.
func TestLineage_Postgres(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)

	slug := tenantSlug("lineage")
	if err := e.Agents().Create(ctx, &loom.Agent{
		Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: "echo",
	}); err != nil {
		t.Fatal(err)
	}

	// A 5-turn playthrough: one session per turn, each forked from the last.
	root := &loom.Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, root, loom.StepRequest{AgentSlug: slug}); err != nil {
		t.Fatal(err)
	}
	chain := []*loom.Session{root}
	cur := root
	for range 4 {
		b, err := e.Sessions().Fork(ctx, cur.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.RunStep(ctx, b, loom.StepRequest{AgentSlug: slug}); err != nil {
			t.Fatal(err)
		}
		chain = append(chain, b)
		cur = b
	}
	head := chain[len(chain)-1]

	// An abandoned rewind branch: in the tree, absent from the head's line.
	if _, err := e.Sessions().Fork(ctx, chain[2].ID, 0); err != nil {
		t.Fatal(err)
	}

	line, err := e.Sessions().Ancestry(ctx, head.ID)
	if err != nil {
		t.Fatalf("Ancestry on Postgres: %v", err)
	}
	if len(line) != len(chain) {
		t.Fatalf("Ancestry = %d sessions, want %d", len(line), len(chain))
	}
	for i, s := range line {
		if s.ID != chain[i].ID {
			t.Fatalf("Ancestry[%d] = %s, want %s (root-first order)", i, s.ID, chain[i].ID)
		}
	}

	tree, err := e.Sessions().BranchTree(ctx, root.ID)
	if err != nil {
		t.Fatalf("BranchTree on Postgres: %v", err)
	}
	if tree.StepCount != 1 {
		t.Fatalf("root StepCount = %d, want 1", tree.StepCount)
	}
	var count func(*loom.BranchNode) int
	count = func(n *loom.BranchNode) int {
		total := 1
		for _, c := range n.Children {
			total += count(c)
		}
		return total
	}
	// 5 chain sessions + 1 abandoned branch.
	if got := count(tree); got != 6 {
		t.Fatalf("tree holds %d nodes, want 6", got)
	}
}

// TestHeaderResumeStepIndex_Postgres covers the cheap resume path on Postgres:
// step indices are derived from the persisted MAX inside the step transaction,
// so a session loaded with GetHeader (History == nil) continues numbering
// instead of restarting at 0 and colliding with UNIQUE(session_id, step_index).
func TestHeaderResumeStepIndex_Postgres(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)

	slug := tenantSlug("resume")
	if err := e.Agents().Create(ctx, &loom.Agent{
		Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: "echo",
	}); err != nil {
		t.Fatal(err)
	}
	sess := &loom.Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	const turns = 6
	for turn := range turns {
		h, err := e.Sessions().GetHeader(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if h.History != nil {
			t.Fatal("GetHeader loaded History; the premise of this test is gone")
		}
		step, err := e.RunStep(ctx, h, loom.StepRequest{AgentSlug: slug})
		if err != nil {
			t.Fatalf("turn %d resuming from a header: %v", turn, err)
		}
		if step.Index != turn {
			t.Fatalf("turn %d step index = %d, want %d", turn, step.Index, turn)
		}
	}

	steps, err := e.Sessions().Steps(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != turns {
		t.Fatalf("persisted steps = %d, want %d", len(steps), turns)
	}
	// Each step keeps its own checkpoint, so a rewind lands where it should.
	for i := range turns {
		if _, found, err := e.Sessions().StateAt(ctx, sess.ID, i); err != nil || !found {
			t.Fatalf("no checkpoint at index %d: found=%v err=%v", i, found, err)
		}
	}
}

// TestLatestCache_Postgres confirms the latest/active pointer cache and its
// eviction behave on Postgres — the caching is Go-level, but this proves it
// composes with real registry writes and stays owner-scoped end to end.
func TestLatestCache_Postgres(t *testing.T) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("LOOM_DSN not set — skipping integration test")
	}
	ctx := context.Background()
	db := openTestDB(t)
	e, err := loom.New(loom.Config{
		DB: db, Dialect: loom.DialectPostgres,
		Generators:     map[string]loom.Generator{"echo": echo.New("[test]")},
		Cache:          loom.NewInProcessCache(),
		LatestCacheTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	slug := tenantSlug("lc")
	if err := e.Agents().Create(ctx, &loom.Agent{
		Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: "echo",
	}); err != nil {
		t.Fatal(err)
	}
	// Prime, then insert v2 out-of-band so only a cache hit could still say v1.
	if got, err := e.Agents().Latest(ctx, "", slug); err != nil || got.Version != 1 {
		t.Fatalf("Latest = v%d err=%v, want v1", got.Version, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO loom_agents (slug, owner, version, modality, generator_slug) VALUES ($1,'',2,'text','echo')`,
		slug); err != nil {
		t.Fatal(err)
	}
	if got, err := e.Agents().Latest(ctx, "", slug); err != nil || got.Version != 1 {
		t.Fatalf("Latest after out-of-band insert = v%d, want cached v1", got.Version)
	}
	// Create v3 through the service evicts; the pointer advances.
	if err := e.Agents().Create(ctx, &loom.Agent{
		Slug: slug, Version: 3, Modal: loom.ModalityText, GeneratorSlug: "echo",
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.Agents().Latest(ctx, "", slug); err != nil || got.Version != 3 {
		t.Fatalf("Latest after Create(v3) = v%d, want 3 (no eviction)", got.Version)
	}

	// Owner isolation: a different owner must not resolve via the cached entry.
	if _, err := e.Agents().Latest(ctx, "other", slug); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("Latest(other) = %v, want ErrNotFound (cache not owner-scoped)", err)
	}
}

// TestFlowValidate_Postgres confirms Flow.Inputs persists (migration 8 column)
// and Flows().Validate resolves each agent's declared variables end to end on
// Postgres — the wiring the phase-1 producer/consumer work adds.
func TestFlowValidate_Postgres(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)

	leadSlug := tenantSlug("lead")
	folSlug := tenantSlug("fol")

	// Follower consumes the lead's output; lead consumes an external input.
	mkAgent := func(slug string, vars []string) {
		ut := &loom.Prompt{Slug: slug + "-ut", Version: 1, Kind: loom.PromptKindUserTemplate,
			Body: "x", Variables: vars}
		if err := e.Prompts().Create(ctx, ut); err != nil {
			t.Fatal(err)
		}
		if err := e.Agents().Create(ctx, &loom.Agent{
			Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: "echo",
			UserTemplateID: ut.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mkAgent(leadSlug, []string{"topic"})
	mkAgent(folSlug, []string{"Lead"})

	rec := &loom.FlowRecord{
		Slug: tenantSlug("flow"), Version: 1, IsActive: true,
		Inputs: []string{"topic"},
		Agents: []loom.FlowAgentEntry{
			{AgentSlug: leadSlug, OutputKey: "Lead"},
			{AgentSlug: folSlug, OutputKey: "Fol"},
		},
	}
	if err := e.Flows().Validate(ctx, rec); err != nil {
		t.Fatalf("valid wiring rejected on Postgres: %v", err)
	}
	if err := e.Flows().Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// Inputs survive the round-trip through the migration-8 column.
	got, err := e.Flows().Get(ctx, "", rec.Slug, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "topic" {
		t.Fatalf("Inputs round-trip on Postgres = %v, want [topic]", got.Inputs)
	}

	// A dangling consumed variable is rejected.
	bad := &loom.FlowRecord{
		Slug: tenantSlug("bad"), Version: 1, IsActive: true,
		Agents: []loom.FlowAgentEntry{{AgentSlug: leadSlug, OutputKey: "Lead"}},
	}
	if err := e.Flows().Validate(ctx, bad); !errors.Is(err, loom.ErrFlowInvalid) {
		t.Fatalf("dangling variable on Postgres: err = %v, want ErrFlowInvalid", err)
	}
}

// TestLayeredExecution_Postgres proves a follower that consumes a sibling's
// output runs after it and receives that output, end to end on Postgres.
func TestLayeredExecution_Postgres(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	// No-prefix echo so an agent's output is exactly its rendered template.
	e, err := loom.New(loom.Config{
		DB: db, Dialect: loom.DialectPostgres,
		Generators: map[string]loom.Generator{"echo": echo.New("")},
	})
	if err != nil {
		t.Fatal(err)
	}

	leadS := tenantSlug("lead")
	f1S := tenantSlug("f1")
	f2S := tenantSlug("f2")
	mk := func(slug, body string, vars []string) {
		ut := &loom.Prompt{Slug: slug + "-ut", Version: 1, Kind: loom.PromptKindUserTemplate,
			Body: body, Variables: vars}
		if err := e.Prompts().Create(ctx, ut); err != nil {
			t.Fatal(err)
		}
		if err := e.Agents().Create(ctx, &loom.Agent{
			Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: "echo",
			UserTemplateID: ut.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(leadS, "LEAD", nil)
	mk(f1S, "F1OUT", nil)
	mk(f2S, "{{.Inputs.F1}}", []string{"F1"}) // consumes f1's output

	sess := &loom.Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	flow := loom.Flow{
		Slug: "t",
		Lead: loom.FlowAgent{AgentSlug: leadS, OutputKey: "Lead"},
		Followers: []loom.FlowAgent{
			{AgentSlug: f1S, OutputKey: "F1"},
			{AgentSlug: f2S, OutputKey: "F2"},
		},
	}
	turn, err := e.RunTurn(ctx, sess, loom.TurnRequest{Flow: flow})
	if err != nil {
		t.Fatal(err)
	}
	if ferr := turn.Errors[f2S]; ferr != nil {
		t.Fatalf("f2 errored — it did not run after f1 on Postgres: %v", ferr)
	}
	if got := loom.ResultText(turn.Followers[f2S].Result); got != "F1OUT" {
		t.Fatalf("f2 output = %q, want %q (did not receive f1's output)", got, "F1OUT")
	}
}

// TestFlowPlan_Postgres confirms the resolved wiring plan (nodes, edges,
// layers) computes end to end on Postgres — the structure a client renders.
func TestFlowPlan_Postgres(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)

	mk := func(slug string, vars []string) {
		ut := &loom.Prompt{Slug: slug + "-ut", Version: 1, Kind: loom.PromptKindUserTemplate,
			Body: "x", Variables: vars}
		if err := e.Prompts().Create(ctx, ut); err != nil {
			t.Fatal(err)
		}
		if err := e.Agents().Create(ctx, &loom.Agent{
			Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: "echo",
			UserTemplateID: ut.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	leadS, f1S, f2S := tenantSlug("lead"), tenantSlug("f1"), tenantSlug("f2")
	mk(leadS, []string{"topic"})
	mk(f1S, []string{"Lead"})
	mk(f2S, []string{"F1"})

	rec := &loom.FlowRecord{
		Slug: tenantSlug("flow"), Version: 1, IsActive: true,
		Inputs: []string{"topic"},
		Agents: []loom.FlowAgentEntry{
			{AgentSlug: leadS, OutputKey: "Lead"},
			{AgentSlug: f1S, OutputKey: "F1"},
			{AgentSlug: f2S, OutputKey: "F2"},
		},
	}
	plan, err := e.Flows().Plan(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Layers != 2 {
		t.Fatalf("Layers = %d, want 2", plan.Layers)
	}
	var f1Lead, f2F1 bool
	for _, ed := range plan.Edges {
		if ed.To == f1S && ed.Var == "Lead" && ed.Source == loom.FlowEdgeAgent && ed.From == leadS {
			f1Lead = true
		}
		if ed.To == f2S && ed.Var == "F1" && ed.Source == loom.FlowEdgeAgent && ed.From == f1S {
			f2F1 = true
		}
		if ed.Source == loom.FlowEdgeUnresolved {
			t.Fatalf("unexpected unresolved edge: %+v", ed)
		}
	}
	if !f1Lead || !f2F1 {
		t.Fatalf("expected Lead→f1 and F1→f2 agent edges; got %+v", plan.Edges)
	}
}
