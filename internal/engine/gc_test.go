package engine

// gc_test.go — hermetic coverage for the destructive branch GC worker, which
// previously had none. These tests prove the safety fixes: T1 no longer deletes
// real (played) branches, T3/T4 honour Pin, T4 is scoped to branches (never
// deletes a root), and DryRun deletes nothing.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/rhaqim/loom/schema"
)

func gcSession(t *testing.T, ctx context.Context, e *Engine) *Session {
	t.Helper()
	s := &Session{PlatformID: "gc-test", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

func gcExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func gcMakeBranch(t *testing.T, db *sql.DB, prefix string, id, parent uuid.UUID) {
	gcExec(t, db, fmt.Sprintf(`UPDATE %ssessions SET parent_session_id=$1 WHERE id=$2`, prefix), parent, id)
}

func gcSetBranchPoint(t *testing.T, db *sql.DB, prefix string, id uuid.UUID, bp int) {
	gcExec(t, db, fmt.Sprintf(`UPDATE %ssessions SET branch_point=$1 WHERE id=$2`, prefix), bp, id)
}

func gcAge(t *testing.T, db *sql.DB, prefix string, id uuid.UUID, age time.Duration) {
	old := time.Now().Add(-age)
	gcExec(t, db, fmt.Sprintf(`UPDATE %ssessions SET created_at=$1, updated_at=$2 WHERE id=$3`, prefix), old, old, id)
}

func gcSoftDelete(t *testing.T, db *sql.DB, prefix string, id uuid.UUID, deletedAt time.Time) {
	gcExec(t, db, fmt.Sprintf(`UPDATE %ssessions SET deleted_at=$1 WHERE id=$2`, prefix), deletedAt, id)
}

func gcTagTest(t *testing.T, db *sql.DB, prefix string, id uuid.UUID) {
	gcExec(t, db, fmt.Sprintf(`UPDATE %ssessions SET tags=$1 WHERE id=$2`, prefix), `["test:true"]`, id)
}

func gcAddStep(t *testing.T, db *sql.DB, prefix string, sessionID uuid.UUID, index int) {
	gcExec(t, db, fmt.Sprintf(`INSERT INTO %ssteps (id, session_id, step_index, agent_id, result_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, prefix),
		uuid.New(), sessionID, index, uuid.New(), uuid.New(), time.Now())
}

func gcExists(t *testing.T, db *sql.DB, prefix string, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %ssessions WHERE id=$1 AND deleted_at IS NULL`, prefix), id).Scan(&n); err != nil {
		t.Fatalf("exists query: %v", err)
	}
	return n > 0
}

func gcTestConfig(dryRun bool) BranchGCConfig {
	return BranchGCConfig{
		DryRun:            dryRun,
		SpeculativeMaxAge: time.Hour,
		StaleMaxIdle:      24 * time.Hour,
		SoftDeleteGrace:   time.Hour,
		TestBranchMaxAge:  time.Hour,
		Dialect:           "sqlite",
	}
}

func TestGC_T1_PreservesRealBranchesDeletesSpeculative(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "gc_t1", map[string]Generator{"g": okGen{}}, PollerConfig{})
	root := gcSession(t, ctx, e)

	// Speculative: old branch, no steps -> should be deleted.
	spec := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, spec.ID, root.ID)
	gcAge(t, db, e.prefix, spec.ID, 2*time.Hour)

	// Real: an old branch that was actually played. Mimic a real Fork — a
	// non-NULL branch_point and a step at index 0 (<= branch_point). The OLD
	// predicate (step_index > branch_point) misclassifies this as speculative and
	// DELETES it; the fix preserves it because the branch has a step. (With a NULL
	// branch_point even the old code preserved it, so the non-NULL value is what
	// makes this test actually discriminate.)
	real := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, real.ID, root.ID)
	gcSetBranchPoint(t, db, e.prefix, real.ID, 5)
	gcAge(t, db, e.prefix, real.ID, 2*time.Hour)
	gcAddStep(t, db, e.prefix, real.ID, 0)

	// Pinned speculative branch -> preserved.
	pinned := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, pinned.ID, root.ID)
	gcAge(t, db, e.prefix, pinned.ID, 2*time.Hour)
	if err := e.Sessions().Pin(ctx, pinned.ID); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// Too-new speculative branch -> preserved.
	fresh := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, fresh.ID, root.ID)

	report, err := NewBranchGCWorker(e, gcTestConfig(false)).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.SpeculativeDeleted != 1 {
		t.Fatalf("T1 deleted %d, want 1", report.SpeculativeDeleted)
	}
	if gcExists(t, db, e.prefix, spec.ID) {
		t.Error("speculative branch should have been deleted")
	}
	for name, id := range map[string]uuid.UUID{"real": real.ID, "pinned": pinned.ID, "fresh": fresh.ID, "root": root.ID} {
		if !gcExists(t, db, e.prefix, id) {
			t.Errorf("%s session was wrongly deleted", name)
		}
	}
}

func TestGC_T2_SoftDeletesStaleBranchesHonorsPin(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "gc_t2", map[string]Generator{"g": okGen{}}, PollerConfig{})
	root := gcSession(t, ctx, e)

	// A played branch (has a step, so T1 skips it) idle past StaleMaxIdle -> T2
	// soft-deletes it.
	stale := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, stale.ID, root.ID)
	gcAddStep(t, db, e.prefix, stale.ID, 1)
	gcAge(t, db, e.prefix, stale.ID, 48*time.Hour) // older than StaleMaxIdle (24h)

	// A pinned stale branch -> preserved.
	pinned := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, pinned.ID, root.ID)
	gcAddStep(t, db, e.prefix, pinned.ID, 1)
	gcAge(t, db, e.prefix, pinned.ID, 48*time.Hour)
	if err := e.Sessions().Pin(ctx, pinned.ID); err != nil {
		t.Fatalf("pin: %v", err)
	}

	report, err := NewBranchGCWorker(e, gcTestConfig(false)).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.StaleSoftDeleted != 1 {
		t.Fatalf("T2 soft-deleted %d, want 1", report.StaleSoftDeleted)
	}
	// stale is SOFT-deleted (deleted_at set, row still present), not hard-deleted.
	var deletedAt sql.NullTime
	if err := db.QueryRow(fmt.Sprintf(`SELECT deleted_at FROM %ssessions WHERE id=$1`, e.prefix), stale.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("query deleted_at: %v", err)
	}
	if !deletedAt.Valid {
		t.Error("stale branch should be soft-deleted (deleted_at set, row preserved)")
	}
	if !gcExists(t, db, e.prefix, pinned.ID) {
		t.Error("pinned stale branch should be preserved")
	}
}

func TestGC_T3_HonorsPin(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "gc_t3", map[string]Generator{"g": okGen{}}, PollerConfig{})
	pastGrace := time.Now().Add(-2 * time.Hour)

	// Soft-deleted past grace, not pinned -> hard-deleted.
	gone := gcSession(t, ctx, e)
	gcSoftDelete(t, db, e.prefix, gone.ID, pastGrace)

	// Soft-deleted past grace, PINNED -> preserved (the fix).
	keep := gcSession(t, ctx, e)
	if err := e.Sessions().Pin(ctx, keep.ID); err != nil {
		t.Fatalf("pin: %v", err)
	}
	gcSoftDelete(t, db, e.prefix, keep.ID, pastGrace)

	report, err := NewBranchGCWorker(e, gcTestConfig(false)).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.GraceHardDeleted != 1 {
		t.Fatalf("T3 deleted %d, want 1", report.GraceHardDeleted)
	}
	var n int
	db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %ssessions WHERE id=$1`, e.prefix), gone.ID).Scan(&n)
	if n != 0 {
		t.Error("unpinned soft-deleted session should have been hard-deleted")
	}
	db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %ssessions WHERE id=$1`, e.prefix), keep.ID).Scan(&n)
	if n != 1 {
		t.Error("pinned session should survive T3")
	}
}

func TestGC_T4_ScopesToBranchesAndHonorsPin(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "gc_t4", map[string]Generator{"g": okGen{}}, PollerConfig{})
	parent := gcSession(t, ctx, e)

	// Test-tagged branch with a step (so T1 doesn't grab it as speculative) -> T4 deletes.
	branch := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, branch.ID, parent.ID)
	gcAge(t, db, e.prefix, branch.ID, 2*time.Hour)
	gcAddStep(t, db, e.prefix, branch.ID, 0)
	gcTagTest(t, db, e.prefix, branch.ID)

	// Test-tagged branch but PINNED -> preserved (the fix).
	pinnedBranch := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, pinnedBranch.ID, parent.ID)
	gcAge(t, db, e.prefix, pinnedBranch.ID, 2*time.Hour)
	gcAddStep(t, db, e.prefix, pinnedBranch.ID, 0)
	gcTagTest(t, db, e.prefix, pinnedBranch.ID)
	if err := e.Sessions().Pin(ctx, pinnedBranch.ID); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// Test-tagged ROOT (no parent) -> preserved (the fix: T4 is branch-scoped).
	rootTagged := gcSession(t, ctx, e)
	gcAge(t, db, e.prefix, rootTagged.ID, 2*time.Hour)
	gcTagTest(t, db, e.prefix, rootTagged.ID)

	// Branch tagged "latest:true" -> preserved. Regression for the substring
	// bug: LIKE '%test:true%' matched "latest:true" and wrongly hard-deleted it.
	// The array-membership predicate must NOT match this branch.
	lookalike := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, lookalike.ID, parent.ID)
	gcAge(t, db, e.prefix, lookalike.ID, 2*time.Hour)
	gcAddStep(t, db, e.prefix, lookalike.ID, 0)
	gcExec(t, db, fmt.Sprintf(`UPDATE %ssessions SET tags=$1 WHERE id=$2`, e.prefix), `["latest:true"]`, lookalike.ID)

	report, err := NewBranchGCWorker(e, gcTestConfig(false)).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.TestBranchesDeleted != 1 {
		t.Fatalf("T4 deleted %d, want 1", report.TestBranchesDeleted)
	}
	if gcExists(t, db, e.prefix, branch.ID) {
		t.Error("test-tagged branch should have been deleted")
	}
	for name, id := range map[string]uuid.UUID{"pinnedBranch": pinnedBranch.ID, "rootTagged": rootTagged.ID, "parent": parent.ID, "lookalike(latest:true)": lookalike.ID} {
		if !gcExists(t, db, e.prefix, id) {
			t.Errorf("%s was wrongly deleted by T4", name)
		}
	}
}

// TestGC_T4_Postgres exercises the T4 test-tag tier on the PRODUCTION dialect:
// the SQLite tests hardcode Dialect:"sqlite", so the Postgres `tags::text LIKE`
// predicate (which deletes real sessions) is otherwise never run. Uses real
// RunStep so the steps satisfy Postgres foreign keys.
func TestGC_T4_Postgres(t *testing.T) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("set LOOM_DSN to run the Postgres GC T4 test")
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
	e, err := New(Config{DB: db, Dialect: DialectPostgres, Generators: map[string]Generator{"g": okGen{}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.Agents().Create(ctx, &Agent{Slug: "gca", Version: 1, Modal: ModalityText, GeneratorSlug: "g"})

	parent := gcSession(t, ctx, e)

	// Tagged branch with a real (FK-valid) step -> Postgres T4 must delete it.
	tagged := gcSession(t, ctx, e)
	if _, err := e.RunStep(ctx, tagged, StepRequest{AgentSlug: "gca", AgentVersion: 1}); err != nil {
		t.Fatal(err)
	}
	gcMakeBranch(t, db, e.prefix, tagged.ID, parent.ID)
	gcAge(t, db, e.prefix, tagged.ID, 2*time.Hour)
	gcTagTest(t, db, e.prefix, tagged.ID)

	// Untagged branch with a step -> T4 (test-tag scoped) must NOT delete it.
	untagged := gcSession(t, ctx, e)
	if _, err := e.RunStep(ctx, untagged, StepRequest{AgentSlug: "gca", AgentVersion: 1}); err != nil {
		t.Fatal(err)
	}
	gcMakeBranch(t, db, e.prefix, untagged.ID, parent.ID)
	gcAge(t, db, e.prefix, untagged.ID, 2*time.Hour)

	cfg := gcTestConfig(false)
	cfg.Dialect = "postgres"
	report, err := NewBranchGCWorker(e, cfg).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.TestBranchesDeleted < 1 {
		t.Fatalf("Postgres T4 deleted %d test branches, want >=1 (tags::text LIKE)", report.TestBranchesDeleted)
	}
	if gcExists(t, db, e.prefix, tagged.ID) {
		t.Error("Postgres T4 did not delete the test-tagged branch")
	}
	if !gcExists(t, db, e.prefix, untagged.ID) {
		t.Error("Postgres T4 wrongly deleted the untagged branch")
	}
}

func TestGC_DryRunDeletesNothing(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "gc_dry", map[string]Generator{"g": okGen{}}, PollerConfig{})
	root := gcSession(t, ctx, e)
	spec := gcSession(t, ctx, e)
	gcMakeBranch(t, db, e.prefix, spec.ID, root.ID)
	gcAge(t, db, e.prefix, spec.ID, 2*time.Hour)

	report, err := NewBranchGCWorker(e, gcTestConfig(true)).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.SpeculativeDeleted != 1 {
		t.Fatalf("dry-run should COUNT 1 speculative, got %d", report.SpeculativeDeleted)
	}
	if !gcExists(t, db, e.prefix, spec.ID) {
		t.Error("dry-run must not delete anything")
	}
}
