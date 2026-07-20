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
	"testing"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
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
