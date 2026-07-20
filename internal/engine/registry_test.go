package engine_test

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

// TestRegistryDeleteAndOwnerSQLite exercises the new Delete (with cache
// invalidation) and the owner column round-trip on agents/prompts/response
// formats, hermetically on in-memory SQLite.
func TestRegistryDeleteAndOwnerSQLite(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:reg_"+uuid.NewString()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := loom.New(loom.Config{DB: db, Dialect: loom.DialectSQLite,
		Generators: map[string]loom.Generator{"g": flowStubGen{}}, Cache: loom.NewInProcessCache()})
	if err != nil {
		t.Fatal(err)
	}
	registryDeleteAndOwner(t, e)
}

// TestRegistryDeleteAndOwnerPostgres runs the same against Postgres (LOOM_DSN).
func TestRegistryDeleteAndOwnerPostgres(t *testing.T) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("set LOOM_DSN to run the Postgres registry test")
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
		Generators: map[string]loom.Generator{"g": flowStubGen{}}, Cache: loom.NewInProcessCache()})
	if err != nil {
		t.Fatal(err)
	}
	registryDeleteAndOwner(t, e)
}

func registryDeleteAndOwner(t *testing.T, e *loom.Engine) {
	t.Helper()
	ctx := context.Background()
	uniq := uuid.NewString()[:8]
	const owner = "studio-x"

	// --- owner round-trips on all three entities ---
	aslug := "a-" + uniq
	if err := e.Agents().Create(ctx, &loom.Agent{Slug: aslug, Owner: owner, Version: 1, Modal: loom.ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	a, err := e.Agents().Get(ctx, owner, aslug, 1) // also primes the cache
	if err != nil {
		t.Fatal(err)
	}
	if a.Owner != "studio-x" {
		t.Fatalf("agent owner not round-tripped: %q", a.Owner)
	}

	pslug := "p-" + uniq
	if err := e.Prompts().Create(ctx, &loom.Prompt{Slug: pslug, Owner: owner, Version: 1, Kind: loom.PromptKindSystem, Body: "x"}); err != nil {
		t.Fatal(err)
	}
	p, err := e.Prompts().Get(ctx, owner, pslug, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Owner != "studio-x" {
		t.Fatalf("prompt owner not round-tripped: %q", p.Owner)
	}

	rfslug := "rf-" + uniq
	rfRec := &loom.ResponseFormatRecord{Slug: rfslug, Owner: owner, Version: 1, Schema: map[string]any{"type": "object"}}
	if err := e.ResponseFormats().Create(ctx, rfRec); err != nil {
		t.Fatal(err)
	}
	rf, err := e.ResponseFormats().GetByID(ctx, rfRec.ID) // primes the by-id ("rf-id") cache + round-trips owner
	if err != nil {
		t.Fatal(err)
	}
	if rf.Owner != "studio-x" {
		t.Fatalf("response format owner not round-tripped: %q", rf.Owner)
	}

	// --- another owner cannot reach these records ---
	// The reads above primed the cache; a cache key that dropped the owner would
	// serve those same rows here, so this also guards the cache, not just the SQL.
	if _, err := e.Agents().Get(ctx, "other", aslug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("agent leaked across owners, got %v", err)
	}
	if _, err := e.Prompts().Get(ctx, "other", pslug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("prompt leaked across owners, got %v", err)
	}
	if _, err := e.ResponseFormats().Get(ctx, "other", rfslug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("response format leaked across owners, got %v", err)
	}
	if as, err := e.Agents().List(ctx, "other", ""); err != nil || len(as) != 0 {
		t.Fatalf("List leaked across owners: %d agents, err %v", len(as), err)
	}

	// --- Delete with version 0 is rejected (not a silent no-op) ---
	if err := e.Agents().Delete(ctx, owner, aslug, 0); err == nil {
		t.Fatal("expected error deleting agent version 0")
	}

	// --- Delete removes the version AND invalidates the cache ---
	if err := e.Agents().Delete(ctx, owner, aslug, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Agents().Get(ctx, owner, aslug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("agent: expected ErrNotFound after delete (stale cache?), got %v", err)
	}
	if err := e.Prompts().Delete(ctx, owner, pslug, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Prompts().Get(ctx, owner, pslug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("prompt: expected ErrNotFound after delete (stale cache?), got %v", err)
	}
	if err := e.ResponseFormats().Delete(ctx, owner, rfslug, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResponseFormats().Get(ctx, owner, rfslug, 1); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("response format: expected ErrNotFound after delete (stale cache?), got %v", err)
	}
	// The by-id cache primed via GetByID above must also be evicted.
	if _, err := e.ResponseFormats().GetByID(ctx, rfRec.ID); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("response format by-id: stale cache after delete, got %v", err)
	}
}
