package engine

// cost_attribution_test.go: regressions for two client-reported costing bugs.
//
//  1. A per-request model override (StepRequest.GeneratorOverride) recorded the
//     agent's default generator as the provider and as the pricing key, so the
//     call was priced against the wrong model. A step reloaded from storage also
//     lost its reported model, so pricing it fell back to the default price.
//  2. UsageSummary.PricingConfigured was true whenever any model had a price,
//     so totals that included placeholder-priced rows were reported as real
//     money (e.g. a dry run on an unpriced generator).

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/rhaqim/loom/schema"
)

// pricedEngine is reproEngine with a pricing table.
func pricedEngine(t *testing.T, name string, gens map[string]Generator, pricing map[string]ModelPrice) *Engine {
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
	e, err := New(Config{DB: db, Dialect: DialectSQLite, Generators: gens, Pricing: pricing})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// modelGen reports a fixed model on result metadata, as real adapters do.
type modelGen struct{ model string }

func (modelGen) Modality() Modality { return ModalityText }
func (g modelGen) Generate(context.Context, GenerateRequest) (Result, error) {
	r := NewTextResult("ok", "stop", 1_000_000, 1_000_000)
	if g.model != "" {
		r.Metadata()["model"] = g.model
	}
	return r, nil
}

func setupAgentSession(t *testing.T, e *Engine) *Session {
	t.Helper()
	ctx := context.Background()
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "default"}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestGeneratorOverrideIsCosted(t *testing.T) {
	ctx := context.Background()
	// Neither generator reports a model, so the generator slug is the pricing key.
	e := pricedEngine(t, "genoverride", map[string]Generator{
		"default": modelGen{}, "premium": modelGen{},
	}, map[string]ModelPrice{
		"default": {InputPer1M: 1, OutputPer1M: 1},
		"premium": {InputPer1M: 10, OutputPer1M: 10},
	})
	sess := setupAgentSession(t, e)

	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a", GeneratorOverride: "premium"}); err != nil {
		t.Fatal(err)
	}
	rows, err := e.Cost().ByStep(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d cost rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Provider != "premium" || r.Model != "premium" {
		t.Fatalf("override recorded as provider=%q model=%q, want premium/premium", r.Provider, r.Model)
	}
	if r.USDCost != 20 || r.Estimated {
		t.Fatalf("override priced at %v (estimated=%v), want 20 from the premium price", r.USDCost, r.Estimated)
	}
}

func TestReloadedStepKeepsReportedModel(t *testing.T) {
	ctx := context.Background()
	e := pricedEngine(t, "reloadmodel", map[string]Generator{"default": modelGen{model: "gpt-override"}},
		map[string]ModelPrice{"gpt-override": {InputPer1M: 5, OutputPer1M: 5}})
	sess := setupAgentSession(t, e)
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}
	got, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 {
		t.Fatalf("history len %d, want 1", len(got.History))
	}
	if m := ResultModel(got.History[0].Result); m != "gpt-override" {
		t.Fatalf("reloaded step model = %q, want gpt-override", m)
	}
	if usd, known := e.PriceResult(ResultModel(got.History[0].Result), 1_000_000, 1_000_000); usd != 10 || !known {
		t.Fatalf("reloaded step priced at %v (known=%v), want 10", usd, known)
	}
}

func TestPricingConfiguredReflectsRows(t *testing.T) {
	ctx := context.Background()
	// Only "priced" has a price. A step on "default" (unpriced, no fallback)
	// is placeholder USD, so totals that include it are not real money.
	e := pricedEngine(t, "pricedrows", map[string]Generator{
		"default": modelGen{}, "priced": modelGen{},
	}, map[string]ModelPrice{"priced": {InputPer1M: 1, OutputPer1M: 1}})
	sess := setupAgentSession(t, e)

	// No rows yet: falls back to the engine config (a price exists).
	u, err := e.Cost().SessionUsage(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !u.PricingConfigured {
		t.Fatal("empty session: PricingConfigured = false despite Config.Pricing")
	}

	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a", GeneratorOverride: "priced"}); err != nil {
		t.Fatal(err)
	}
	if u, err = e.Cost().SessionUsage(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if !u.PricingConfigured {
		t.Fatal("only priced rows: PricingConfigured = false")
	}

	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}
	if u, err = e.Cost().SessionUsage(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if u.PricingConfigured {
		t.Fatalf("a placeholder-priced row is included, yet PricingConfigured = true (TotalUSD=%v)", u.TotalUSD)
	}
}
