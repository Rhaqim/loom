package engine

// latestcache_test.go — §1: the read-through cache must cover the latest/active
// resolvers (Agents/Prompts Latest, Flows LatestActive), which are what an
// embedder running "at latest" actually calls, and must evict them on write.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/loom/schema"
	_ "modernc.org/sqlite"
)

func cachedEngine(t *testing.T, name string, latestTTL time.Duration) (*Engine, *sql.DB) {
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
	e, err := New(Config{
		DB: db, Dialect: DialectSQLite,
		Generators:     map[string]Generator{"g": okGen{}},
		Cache:          NewInProcessCache(),
		LatestCacheTTL: latestTTL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, db
}

// rawBumpAgentVersion inserts a higher agent version DIRECTLY, bypassing the
// service so no eviction fires — the way to prove a subsequent Latest was
// served from cache rather than re-reading.
func rawInsertAgent(t *testing.T, db *sql.DB, owner, slug string, version int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO loom_agents (id, slug, owner, version, modality, generator_slug, created_at)
		VALUES (?,?,?,?,?,?,?)`, uuid.NewString(), slug, owner, version, "text", "g", time.Now())
	if err != nil {
		t.Fatal(err)
	}
}

func TestLatestCache_AgentServedAndEvicted(t *testing.T) {
	ctx := context.Background()
	e, db := cachedEngine(t, "lc_agent", time.Minute)

	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	// Prime the cache.
	got, err := e.Agents().Latest(ctx, "", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("Latest = v%d, want 1", got.Version)
	}

	// Insert v2 out-of-band (no eviction). A cache HIT must still return v1.
	rawInsertAgent(t, db, "", "a", 2)
	got, err = e.Agents().Latest(ctx, "", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("Latest after out-of-band insert = v%d, want 1 (cache was not used)", got.Version)
	}

	// Create v3 THROUGH the service — this evicts, so Latest re-reads and now
	// sees the highest version present (v3).
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 3, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	got, err = e.Agents().Latest(ctx, "", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 3 {
		t.Fatalf("Latest after Create(v3) = v%d, want 3 (Create did not evict)", got.Version)
	}
}

// Get(version 0) is the default hot-path posture and must share the cached
// pointer with Latest.
func TestLatestCache_VersionZeroUsesLatestCache(t *testing.T) {
	ctx := context.Background()
	e, db := cachedEngine(t, "lc_v0", time.Minute)
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Agents().Get(ctx, "", "a", 0); err != nil {
		t.Fatal(err)
	}
	rawInsertAgent(t, db, "", "a", 2)
	got, err := e.Agents().Get(ctx, "", "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("Get(v0) = v%d, want 1 (version 0 did not use the latest cache)", got.Version)
	}
}

// The cache must be keyed by owner: one tenant's latest must never satisfy
// another's, or the cache defeats the SQL owner filter.
func TestLatestCache_OwnerScoped(t *testing.T) {
	ctx := context.Background()
	e, _ := cachedEngine(t, "lc_owner", time.Minute)
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Owner: "t1", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Agents().Latest(ctx, "t1", "a"); err != nil {
		t.Fatal(err)
	}
	// t2 has no such agent; a shared key would serve t1's cached record.
	if _, err := e.Agents().Latest(ctx, "t2", "a"); err == nil {
		t.Fatal("Latest(t2) resolved via t1's cached entry — key is not owner-scoped")
	}
}

func TestLatestCache_PromptEvictedOnCreate(t *testing.T) {
	ctx := context.Background()
	e, db := cachedEngine(t, "lc_prompt", time.Minute)
	if err := e.Prompts().Create(ctx, &Prompt{Slug: "p", Version: 1, Kind: PromptKindSystem, Body: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Prompts().Latest(ctx, "", "p"); err != nil {
		t.Fatal(err)
	}
	// Out-of-band v2 → cache still v1.
	if _, err := db.Exec(`INSERT INTO loom_prompts (id, slug, owner, version, kind, body, created_at) VALUES (?,?,?,?,?,?,?)`,
		uuid.NewString(), "p", "", 2, "system", "two", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Prompts().Latest(ctx, "", "p")
	if got.Body != "one" {
		t.Fatalf("prompt Latest = %q, want cached %q", got.Body, "one")
	}
	// Create v3 through the service evicts.
	if err := e.Prompts().Create(ctx, &Prompt{Slug: "p", Version: 3, Kind: PromptKindSystem, Body: "three"}); err != nil {
		t.Fatal(err)
	}
	got, _ = e.Prompts().Latest(ctx, "", "p")
	if got.Body != "three" {
		t.Fatalf("prompt Latest after Create = %q, want %q", got.Body, "three")
	}
}

func TestLatestCache_FlowActiveEvictedOnSetActive(t *testing.T) {
	ctx := context.Background()
	e, _ := cachedEngine(t, "lc_flow", time.Minute)
	mustAgent(t, e, "lead", "g")
	mk := func(v int) {
		if err := e.Flows().Create(ctx, &FlowRecord{Slug: "f", Version: v, IsActive: true,
			Agents: []FlowAgentEntry{{AgentSlug: "lead", OutputKey: "Lead"}}}); err != nil {
			t.Fatal(err)
		}
	}
	mk(1)
	mk(2) // both active, so latest-active is v2

	got, err := e.Flows().LatestActive(ctx, "", "f")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 {
		t.Fatalf("LatestActive = v%d, want 2", got.Version)
	}

	// Roll back to v1 — SetActive must evict, so the next resolve sees v1.
	if err := e.Flows().SetActive(ctx, "", "f", 1); err != nil {
		t.Fatal(err)
	}
	got, err = e.Flows().LatestActive(ctx, "", "f")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("LatestActive after SetActive(1) = v%d, want 1 (SetActive did not evict)", got.Version)
	}
}

// A negative LatestCacheTTL disables latest caching while leaving versioned
// caching intact — the escape hatch for embedders that cannot tolerate any
// cross-instance staleness after a publish.
func TestLatestCache_DisabledByNegativeTTL(t *testing.T) {
	ctx := context.Background()
	e, db := cachedEngine(t, "lc_off", -1)
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Agents().Latest(ctx, "", "a"); err != nil {
		t.Fatal(err)
	}
	// With caching off, an out-of-band v2 is visible immediately.
	rawInsertAgent(t, db, "", "a", 2)
	got, err := e.Agents().Latest(ctx, "", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 {
		t.Fatalf("Latest with caching disabled = v%d, want 2 (it was cached anyway)", got.Version)
	}

	// Versioned caching still works: a concrete-version Get is cached.
	if _, err := e.Agents().Get(ctx, "", "a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM loom_agents WHERE slug='a' AND version=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Agents().Get(ctx, "", "a", 1); err != nil {
		t.Fatalf("versioned cache was disabled too: %v", err)
	}
}
