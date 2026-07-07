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

// TestSessionUpdateOptimisticConcurrencySQLite verifies that a stale writer is
// rejected with ErrSessionConflict instead of silently clobbering a newer write.
func TestSessionUpdateOptimisticConcurrencySQLite(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:cas_"+uuid.NewString()+"?mode=memory&cache=shared")
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
	sessionOptimisticConcurrency(t, e)
}

// TestSessionUpdateOptimisticConcurrencyPostgres runs the same against Postgres.
func TestSessionUpdateOptimisticConcurrencyPostgres(t *testing.T) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("set LOOM_DSN to run the Postgres optimistic-concurrency test")
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
	sessionOptimisticConcurrency(t, e)
}

func sessionOptimisticConcurrency(t *testing.T, e *loom.Engine) {
	ctx := context.Background()
	sess := &loom.Session{PlatformID: "p", State: loom.State{Modality: loom.ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Two writers load the same version.
	a, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != 0 || b.Version != 0 {
		t.Fatalf("initial versions a=%d b=%d, want 0", a.Version, b.Version)
	}

	// First writer wins and bumps the version.
	a.State.Vars = map[string]any{"who": "a"}
	if err := e.Sessions().Update(ctx, a); err != nil {
		t.Fatalf("update a: %v", err)
	}
	if a.Version != 1 {
		t.Fatalf("a.Version = %d, want 1 after a successful update", a.Version)
	}

	// The stale second writer must be rejected, not silently clobber a's write.
	b.State.Vars = map[string]any{"who": "b"}
	if err := e.Sessions().Update(ctx, b); !errors.Is(err, loom.ErrSessionConflict) {
		t.Fatalf("update b err = %v, want ErrSessionConflict", err)
	}

	// The winner's write survived.
	fresh, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State.Vars["who"] != "a" {
		t.Fatalf("persisted state = %v, want a's write", fresh.State.Vars)
	}
	if fresh.Version != 1 {
		t.Fatalf("fresh.Version = %d, want 1", fresh.Version)
	}

	// Re-reading lets the loser retry successfully on the current version.
	fresh.State.Vars = map[string]any{"who": "b2"}
	if err := e.Sessions().Update(ctx, fresh); err != nil {
		t.Fatalf("retry update after re-read: %v", err)
	}
	if fresh.Version != 2 {
		t.Fatalf("fresh.Version = %d, want 2", fresh.Version)
	}

	// Pin is owned by Pin/Unpin; a stale state Update must not clear it.
	if err := e.Sessions().Pin(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	fresh.State.Vars = map[string]any{"who": "b3"}
	if err := e.Sessions().Update(ctx, fresh); err != nil {
		t.Fatalf("update after pin: %v", err)
	}
	cur, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.Pinned {
		t.Fatal("a state Update clobbered the pin flag")
	}

	// A soft-deleted session is no longer updatable.
	if err := e.Sessions().Discard(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	cur.State.Vars = map[string]any{"who": "b4"}
	if err := e.Sessions().Update(ctx, cur); !errors.Is(err, loom.ErrNotFound) {
		t.Fatalf("update of a discarded session err = %v, want ErrNotFound", err)
	}
}
