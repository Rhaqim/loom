package harness

// harness_test.go — proves the VariantMatrix is no longer cosmetic: each variant
// actually runs its own provider/params/prompt. Before the fix runVariant ignored
// the variant config, so every cell ran the identical agent under a different
// label.

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/schema"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// markerGen returns a fixed marker string so a test can tell which generator ran.
type markerGen struct{ marker string }

func (g markerGen) Modality() loom.Modality { return loom.ModalityText }
func (g markerGen) Generate(context.Context, loom.GenerateRequest) (loom.Result, error) {
	return loom.NewTextResult(g.marker, "stop", 1, 1), nil
}

func TestExpandVariants(t *testing.T) {
	// Providers only -> one variant each, NO param override (the footgun fix).
	vs := expandVariants(VariantMatrix{Providers: []string{"a", "b"}})
	if len(vs) != 2 {
		t.Fatalf("providers-only: want 2 variants, got %d", len(vs))
	}
	for _, v := range vs {
		if v.Params != nil {
			t.Errorf("params must be nil when no Params axis, got %+v", v.Params)
		}
	}

	// No axes at all -> a single default variant.
	vs = expandVariants(VariantMatrix{})
	if len(vs) != 1 || vs[0].Provider != "" || vs[0].Params != nil || vs[0].SystemPrompt != nil {
		t.Errorf("default variant wrong: %+v", vs)
	}

	// Providers x Params -> cross product with params populated.
	vs = expandVariants(VariantMatrix{Providers: []string{"a"}, Params: []loom.GenerateParams{{Temperature: 0.1}, {Temperature: 0.9}}})
	if len(vs) != 2 || vs[0].Params == nil || vs[1].Params == nil {
		t.Fatalf("params axis not expanded: %+v", vs)
	}
	if vs[0].Params.Temperature != 0.1 || vs[1].Params.Temperature != 0.9 {
		t.Errorf("param temps wrong: %v / %v", vs[0].Params.Temperature, vs[1].Params.Temperature)
	}

	// SystemPrompts axis -> populated, params stay nil.
	vs = expandVariants(VariantMatrix{Providers: []string{"a"}, SystemPrompts: []loom.PromptRef{{Slug: "p1"}, {Slug: "p2"}}})
	if len(vs) != 2 || vs[0].SystemPrompt == nil || vs[0].SystemPrompt.Slug != "p1" {
		t.Fatalf("system-prompt axis not expanded: %+v", vs)
	}
	if vs[0].Params != nil {
		t.Errorf("params must stay nil when only SystemPrompts axis is given")
	}
}

func TestRunRejectsOversizeVariantMatrix(t *testing.T) {
	providers := make([]string, maxVariants+1)
	for i := range providers {
		providers[i] = fmt.Sprintf("p%d", i)
	}
	plan := &TestPlan{
		Name:     "too-big",
		Session:  SessionScript{PlatformID: "t", Steps: []ScriptedStep{{AgentSlug: "x"}}},
		Variants: VariantMatrix{Providers: providers},
	}
	// The cap is enforced before any variant runs, so the engine is never used.
	if _, err := Run(context.Background(), nil, plan); err == nil {
		t.Fatal("expected oversize variant matrix to be rejected")
	}
}

func TestRunAppliesProviderOverride(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:harness_"+uuid.NewString()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := loom.New(loom.Config{
		DB:      db,
		Dialect: loom.DialectSQLite,
		Generators: map[string]loom.Generator{
			"a": markerGen{"AAA"},
			"b": markerGen{"BBB"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Agent's default generator is "a"; the "b" variant must override it.
	if err := e.Agents().Create(ctx, &loom.Agent{
		ID: uuid.New(), Slug: "h-agent", Version: 1, Modal: loom.ModalityText, GeneratorSlug: "a",
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []string
	collect := Custom("collect", func(step *loom.Step, result loom.Result) error {
		if tr, ok := result.(*loom.TextResult); ok {
			mu.Lock()
			seen = append(seen, tr.Content)
			mu.Unlock()
		}
		return nil
	})

	plan := &TestPlan{
		Name:       "override",
		Session:    SessionScript{PlatformID: "t", Steps: []ScriptedStep{{AgentSlug: "h-agent"}}},
		Variants:   VariantMatrix{Providers: []string{"a", "b"}},
		Assertions: []Assertion{collect},
	}
	if _, err := Run(ctx, e, plan); err != nil {
		t.Fatal(err)
	}

	// Both generators must have run — proving the provider override is applied.
	// Before the fix both variants used "a", so only "AAA" would appear.
	gotA, gotB := false, false
	for _, s := range seen {
		switch s {
		case "AAA":
			gotA = true
		case "BBB":
			gotB = true
		}
	}
	if !gotA || !gotB {
		t.Errorf("variants did not use distinct providers: seen=%v (want both AAA and BBB)", seen)
	}
}
