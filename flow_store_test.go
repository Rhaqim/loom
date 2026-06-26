package loom_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/schema"
	_ "modernc.org/sqlite"
)

type flowStubGen struct{}

func (flowStubGen) Modality() loom.Modality { return loom.ModalityText }
func (flowStubGen) Generate(context.Context, loom.GenerateRequest) (loom.Result, error) {
	return loom.NewTextResult("ok", "stop", 1, 1), nil
}

// TestFlowRoundTripSQLite runs the full persisted-flow lifecycle against an
// in-memory SQLite DB (hermetic, no external infra) — this exercises the SQLite
// row-level path (single-connection QueryRow-then-Query, INT bools, TEXT ids and
// JSON params, child-first delete).
func TestFlowRoundTripSQLite(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:flowrt_"+uuid.NewString()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := loom.New(loom.Config{DB: db, Dialect: loom.DialectSQLite,
		Generators: map[string]loom.Generator{"g": flowStubGen{}}})
	if err != nil {
		t.Fatal(err)
	}
	flowRoundTrip(t, e)
}

// TestFlowRoundTripPostgres runs the same lifecycle against Postgres. Gated on
// LOOM_DSN; uses unique slugs so it is safe to re-run against a shared DB.
func TestFlowRoundTripPostgres(t *testing.T) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("set LOOM_DSN to run the Postgres flow round-trip")
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
	e, err := loom.New(loom.Config{DB: db, Dialect: loom.DialectPostgres,
		Generators: map[string]loom.Generator{"g": flowStubGen{}}})
	if err != nil {
		t.Fatal(err)
	}
	flowRoundTrip(t, e)
}

// flowRoundTrip exercises persist -> read-back -> run-from-record -> delete, plus
// the Create/Delete validation, with identical assertions on both dialects.
func flowRoundTrip(t *testing.T, e *loom.Engine) {
	t.Helper()
	ctx := context.Background()
	uniq := uuid.NewString()[:8]
	lead := "lead-" + uniq
	follower := "fol-" + uniq
	for _, slug := range []string{lead, follower} {
		if err := e.Agents().Create(ctx, &loom.Agent{Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: "g"}); err != nil {
			t.Fatal(err)
		}
	}

	flowSlug := "flow-" + uniq
	if err := e.Flows().Create(ctx, &loom.FlowRecord{
		Owner: "", Slug: flowSlug, Version: 1, Category: "story", IsActive: true,
		Agents: []loom.FlowAgentEntry{
			{AgentSlug: lead, AgentVersion: 1},
			{AgentSlug: follower, AgentVersion: 1, OutputKey: "Critic", Params: map[string]any{"temperature": 0.5}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := e.Flows().Get(ctx, "", flowSlug, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != flowSlug || got.Version != 1 || got.Owner != "" || got.Category != "story" || !got.IsActive {
		t.Fatalf("flow header mismatch: %+v", got)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("want 2 agents, got %d", len(got.Agents))
	}
	if got.Agents[0].AgentSlug != lead || got.Agents[0].Position != 0 {
		t.Fatalf("lead not first: %+v", got.Agents[0])
	}
	if got.Agents[1].AgentSlug != follower || got.Agents[1].Position != 1 || got.Agents[1].OutputKey != "Critic" {
		t.Fatalf("follower mismatch: %+v", got.Agents[1])
	}
	if v, _ := got.Agents[1].Params["temperature"].(float64); v != 0.5 {
		t.Fatalf("params not round-tripped: %+v", got.Agents[1].Params)
	}

	// .Flow() builds a lead-first runtime Flow.
	f := got.Flow()
	if f.Lead.AgentSlug != lead || len(f.Followers) != 1 || f.Followers[0].AgentSlug != follower {
		t.Fatalf("Flow() build wrong: %+v", f)
	}

	// version 0 = latest.
	if _, err := e.Flows().Get(ctx, "", flowSlug, 0); err != nil {
		t.Fatalf("latest: %v", err)
	}
	// List returns the flow header (without agents).
	list, err := e.Flows().List(ctx, "", "story")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, fr := range list {
		if fr.Slug == flowSlug {
			found = true
		}
	}
	if !found {
		t.Fatalf("List did not return the created flow")
	}

	// Validation: empty agents, version 0 on Create, and version 0 on Delete are rejected.
	if err := e.Flows().Create(ctx, &loom.FlowRecord{Slug: "bad", Version: 1}); err == nil {
		t.Fatal("expected error creating a flow with no agents")
	}
	if err := e.Flows().Create(ctx, &loom.FlowRecord{Slug: "bad", Version: 0, Agents: []loom.FlowAgentEntry{{AgentSlug: lead}}}); err == nil {
		t.Fatal("expected error creating a flow with version 0")
	}
	if err := e.Flows().Delete(ctx, "", flowSlug, 0); err == nil {
		t.Fatal("expected error deleting with version 0")
	}

	// Execute the turn straight from the saved record.
	sess := &loom.Session{PlatformID: "p", State: loom.State{Modality: loom.ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	turn, err := e.RunTurnBySlug(ctx, sess, "", flowSlug, 1, loom.TurnRequest{})
	if err != nil {
		t.Fatalf("RunTurnBySlug: %v", err)
	}
	if turn.Lead == nil || len(turn.Followers) != 1 {
		t.Fatalf("turn did not run lead + follower: %+v", turn)
	}

	// Delete removes the version.
	if err := e.Flows().Delete(ctx, "", flowSlug, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Flows().Get(ctx, "", flowSlug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}
