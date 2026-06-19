package featuretest

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/schema"
	_ "modernc.org/sqlite"
)

// leadGen streams two words so the bus carries chunks on the "lead" topic.
type leadGen struct{}

func (leadGen) Modality() loom.Modality { return loom.ModalityText }
func (leadGen) Generate(_ context.Context, _ loom.GenerateRequest) (loom.Result, error) {
	return loom.NewTextResult("alpha beta", "stop", 1, 1), nil
}
func (leadGen) GenerateStream(_ context.Context, _ loom.GenerateRequest) (<-chan loom.Chunk, <-chan loom.Result, error) {
	chunks := make(chan loom.Chunk, 2)
	results := make(chan loom.Result, 1)
	go func() {
		defer close(chunks)
		defer close(results)
		chunks <- loom.Chunk{Content: "alpha"}
		chunks <- loom.Chunk{Content: "beta"}
		results <- loom.NewTextResult("alpha beta", "stop", 1, 1)
	}()
	return chunks, results, nil
}

// followerGen reads the lead's messages off the bus and records the params it saw.
type followerGen struct {
	heard   *string
	temp    *float64
	tension *any
}

func (g followerGen) Modality() loom.Modality { return loom.ModalityText }
func (g followerGen) Generate(_ context.Context, req loom.GenerateRequest) (loom.Result, error) {
	*g.temp = req.Params.Temperature
	if req.ParamsMap != nil {
		*g.tension = req.ParamsMap["tension"]
	}
	if req.Bus != nil {
		ch := req.Bus.Subscribe("lead") // replay: lead already streamed before we started
		var parts []string
	read:
		for {
			select {
			case m := <-ch:
				if s, ok := m.Data.(string); ok {
					parts = append(parts, s)
				}
			case <-time.After(200 * time.Millisecond):
				break read
			}
		}
		*g.heard = strings.Join(parts, "|")
	}
	return loom.NewTextResult("ok", "stop", 1, 1), nil
}

func TestCrossAgentBusAndParamsMap(t *testing.T) {
	ctx := context.Background()
	db, _ := sql.Open("sqlite", "file:feat?mode=memory&cache=shared")
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}

	var heard string
	var temp float64
	var tension any
	e, err := loom.New(loom.Config{
		DB: db, Dialect: loom.DialectSQLite,
		Generators: map[string]loom.Generator{
			"lead":     leadGen{},
			"follower": followerGen{heard: &heard, temp: &temp, tension: &tension},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Minimal agents (no prompts needed).
	mustAgent(t, ctx, e, "author", "lead")
	mustAgent(t, ctx, e, "critic", "follower")

	sess := &loom.Session{PlatformID: "p", State: loom.State{Modality: loom.ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	turn, err := e.RunTurn(ctx, sess, loom.TurnRequest{
		Flow: loom.Flow{
			Slug:      "t",
			Lead:      loom.FlowAgent{AgentSlug: "author", Stream: true},
			Followers: []loom.FlowAgent{{AgentSlug: "critic"}},
		},
		// Params map carries a model knob (temperature) and a domain knob (tension).
		Params: map[string]any{"temperature": 0.7, "tension": 0.6},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cross-agent communication: the follower received the lead's streamed chunks.
	if !strings.Contains(heard, "alpha") || !strings.Contains(heard, "beta") {
		t.Errorf("follower did not receive lead's bus messages, got %q", heard)
	}
	// Params map: model key folded onto typed params; domain key carried through.
	if temp != 0.7 {
		t.Errorf("temperature param not folded onto typed params: got %v", temp)
	}
	if tension != 0.6 {
		t.Errorf("domain param 'tension' not delivered via ParamsMap: got %v", tension)
	}
	// Bus log is exposed on the Turn.
	if len(turn.Messages) == 0 {
		t.Error("expected turn.Messages to capture bus traffic")
	}
	t.Logf("OK heard=%q temp=%v tension=%v messages=%d", heard, temp, tension, len(turn.Messages))
}

func mustAgent(t *testing.T, ctx context.Context, e *loom.Engine, slug, gen string) {
	t.Helper()
	if err := e.Agents().Create(ctx, &loom.Agent{Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: gen}); err != nil {
		t.Fatal(err)
	}
}

// jsonGen returns a fixed JSON string as a TextResult.
type jsonGen struct{ out string }

func (jsonGen) Modality() loom.Modality { return loom.ModalityText }
func (g jsonGen) Generate(_ context.Context, _ loom.GenerateRequest) (loom.Result, error) {
	return loom.NewTextResult(g.out, "stop", 1, 1), nil
}

func TestSchemaValidationHook(t *testing.T) {
	ctx := context.Background()
	db, _ := sql.Open("sqlite", "file:schema?mode=memory&cache=shared")
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := loom.New(loom.Config{DB: db, Dialect: loom.DialectSQLite,
		Generators: map[string]loom.Generator{
			"bad":  jsonGen{out: `{"nope": true}`},        // missing required "status"
			"good": jsonGen{out: `{"status": "ONGOING"}`}, // valid
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.Hooks().RegisterPost("schema", loom.SchemaValidationPostHook())

	rf := loom.MustResponseFormatJSON(`{
		"type":"object",
		"properties":{"status":{"type":"string","enum":["ONGOING","ENDED"]}},
		"required":["status"],
		"additionalProperties":false
	}`, true)

	mk := func(slug, gen string) {
		if err := e.Agents().Create(ctx, &loom.Agent{Slug: slug, Version: 1, Modal: loom.ModalityText, GeneratorSlug: gen, ResponseFormat: rf}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a-bad", "bad")
	mk("a-good", "good")

	sess := &loom.Session{PlatformID: "p", State: loom.State{Modality: loom.ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Invalid output: validated, retried, and (since it never fixes) fails.
	if _, err := e.RunStep(ctx, sess, loom.StepRequest{AgentSlug: "a-bad", MaxRetries: 1}); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("expected schema-validation failure, got %v", err)
	}
	// Valid output passes.
	if _, err := e.RunStep(ctx, sess, loom.StepRequest{AgentSlug: "a-good"}); err != nil {
		t.Fatalf("valid output should pass: %v", err)
	}
}

func TestPerModelPricing(t *testing.T) {
	ctx := context.Background()
	db, _ := sql.Open("sqlite", "file:price?mode=memory&cache=shared")
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := loom.New(loom.Config{DB: db, Dialect: loom.DialectSQLite,
		Generators: map[string]loom.Generator{"g": jsonGen{out: "ok"}},
		Pricing:    map[string]loom.ModelPrice{"gpt-4o-mini": {InputPer1M: 0.15, OutputPer1M: 0.60}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 1,000,000 in + 1,000,000 out at 0.15/0.60 => $0.75.
	if got := e.PriceResult("gpt-4o-mini", 1_000_000, 1_000_000); got != 0.75 {
		t.Errorf("per-model price wrong: got %v want 0.75", got)
	}
	// Unknown model falls back to the built-in flat default ($1/$3) => $4.
	if got := e.PriceResult("mystery", 1_000_000, 1_000_000); got != 4.0 {
		t.Errorf("fallback price wrong: got %v want 4.0", got)
	}
}

// Two agents reference the SAME response-format record; both get the schema, and
// the record is stored once (reused, not copied).
func TestSharedResponseFormatReference(t *testing.T) {
	ctx := context.Background()
	db, _ := sql.Open("sqlite", "file:rfref?mode=memory&cache=shared")
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	g := &recParamGen{}
	e, err := loom.New(loom.Config{DB: db, Dialect: loom.DialectSQLite,
		Generators: map[string]loom.Generator{"g": g}})
	if err != nil {
		t.Fatal(err)
	}
	// One reusable response-format record.
	rf := loom.MustResponseFormatJSON(`{"type":"object","properties":{"x":{"type":"string"}}}`, true)
	rec := &loom.ResponseFormatRecord{Slug: "shared", Version: 1, Schema: rf.Schema, StrictMode: rf.StrictMode}
	if err := e.ResponseFormats().Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// Two agents (e.g. system-prompt v1 and v2) reference the SAME record.
	for _, v := range []int{1, 2} {
		id := rec.ID
		if err := e.Agents().Create(ctx, &loom.Agent{Slug: "a", Version: v, Modal: loom.ModalityText, GeneratorSlug: "g", ResponseFormatID: &id}); err != nil {
			t.Fatal(err)
		}
	}
	sess := &loom.Session{PlatformID: "p", State: loom.State{Modality: loom.ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// Both agent versions resolve the referenced schema into the request.
	for _, v := range []int{1, 2} {
		g.lastRF = nil
		if _, err := e.RunStep(ctx, sess, loom.StepRequest{AgentSlug: "a", AgentVersion: v}); err != nil {
			t.Fatal(err)
		}
		if g.lastRF == nil || g.lastRF.Schema["type"] != "object" {
			t.Fatalf("agent v%d did not resolve the referenced response format", v)
		}
	}
	// Stored once.
	got, err := e.ResponseFormats().Get(ctx, "shared", 1)
	if err != nil || got.ID != rec.ID {
		t.Fatalf("response format not retrievable as a single record: %v", err)
	}
}

type recParamGen struct{ lastRF *loom.ResponseFormat }

func (g *recParamGen) Modality() loom.Modality { return loom.ModalityText }
func (g *recParamGen) Generate(_ context.Context, req loom.GenerateRequest) (loom.Result, error) {
	g.lastRF = req.ResponseFormat
	return loom.NewTextResult("ok", "stop", 1, 1), nil
}

func TestValidateJSONSchemaUnit(t *testing.T) {
	sch := map[string]any{
		"type":     "object",
		"required": []any{"n"},
		"properties": map[string]any{
			"n": map[string]any{"type": "integer", "minimum": float64(0)},
		},
		"additionalProperties": false,
	}
	if errs := loom.ValidateJSONSchema(sch, map[string]any{"n": float64(3)}); len(errs) != 0 {
		t.Errorf("valid value rejected: %v", errs)
	}
	if errs := loom.ValidateJSONSchema(sch, map[string]any{"x": float64(1)}); len(errs) == 0 {
		t.Error("missing-required + unexpected-property not caught")
	}
}

// busHookGen does nothing special; the hook does the bus work.
func TestBusAccessibleFromHook(t *testing.T) {
	ctx := context.Background()
	db, _ := sql.Open("sqlite", "file:bushook?mode=memory&cache=shared")
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := loom.New(loom.Config{DB: db, Dialect: loom.DialectSQLite,
		Generators: map[string]loom.Generator{"g": jsonGen{out: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	// A post-hook publishes a QA verdict on the turn bus — bus-driven QA.
	e.Hooks().RegisterPost("bus-qa", func(_ context.Context, req *loom.StepRequest, res loom.Result) (loom.Result, error) {
		if b := req.Bus(); b != nil {
			b.Publish("qa", "qa.verdict", "looks good")
		}
		return res, nil
	})
	mustAgent(t, ctx, e, "lead", "g")
	mustAgent(t, ctx, e, "f", "g")
	sess := &loom.Session{PlatformID: "p", State: loom.State{Modality: loom.ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	turn, err := e.RunTurn(ctx, sess, loom.TurnRequest{Flow: loom.Flow{
		Slug: "t", Lead: loom.FlowAgent{AgentSlug: "lead"}, Followers: []loom.FlowAgent{{AgentSlug: "f"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var sawQA bool
	for _, m := range turn.Messages {
		if m.Topic == "qa.verdict" {
			sawQA = true
		}
	}
	if !sawQA {
		t.Error("hook could not publish on the turn bus")
	}
}
