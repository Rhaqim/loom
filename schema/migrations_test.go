package schema

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

func openMemSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:mig_"+uuid.NewString()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	// In-memory shared-cache DBs vanish when the last connection closes; pin to
	// one connection so the schema survives for the whole test.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func hasColumn(t *testing.T, db *sql.DB, d Dialect, table, column string) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ok, err := columnExists(ctx, tx, d, table, column)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestApplyFreshSQLite checks a clean database ends up at the latest version
// with the owner column present on a versioned registry table.
func TestApplyFreshSQLite(t *testing.T) {
	ctx := context.Background()
	db := openMemSQLite(t)
	l := NewLoader(DialectSQLite)

	if err := l.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}

	applied, err := l.appliedVersions(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations() {
		if !applied[m.Version] {
			t.Fatalf("migration %d (%s) not recorded as applied", m.Version, m.Name)
		}
	}
	if got, want := len(applied), len(migrations()); got != want {
		t.Fatalf("applied versions = %d, want %d", got, want)
	}
	if !hasColumn(t, db, DialectSQLite, "loom_prompts", "owner") {
		t.Fatal("loom_prompts is missing the owner column after Apply")
	}
}

// TestApplyIdempotentSQLite checks that a second Apply is a no-op and does not
// re-run or duplicate migrations.
func TestApplyIdempotentSQLite(t *testing.T) {
	ctx := context.Background()
	db := openMemSQLite(t)
	l := NewLoader(DialectSQLite)

	if err := l.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ctx, db); err != nil {
		t.Fatalf("second Apply failed: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+l.ledgerTable()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations()) {
		t.Fatalf("ledger has %d rows after two Applys, want %d", count, len(migrations()))
	}
}

// TestAdoptPreMigrationSQLite is the retroactive-fix case: a database created
// before the migration framework, holding registry tables that predate the
// owner column. Apply must adopt them and backfill the missing column on every
// table in the owner-scope migration, not just one.
func TestAdoptPreMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	db := openMemSQLite(t)

	// Stand in for an old database: every owner-scoped registry table exists
	// without an owner column, and there is no ledger. Guards the full backfill
	// list so dropping any table from migration 2 would fail this test.
	owned := []string{"loom_prompts", "loom_response_formats", "loom_agents", "loom_flows"}
	for _, table := range owned {
		if _, err := db.ExecContext(ctx, `CREATE TABLE `+table+` (
			id      TEXT PRIMARY KEY,
			slug    TEXT NOT NULL,
			version INTEGER NOT NULL
		)`); err != nil {
			t.Fatal(err)
		}
		if hasColumn(t, db, DialectSQLite, table, "owner") {
			t.Fatalf("precondition: %s should not have owner yet", table)
		}
	}

	l := NewLoader(DialectSQLite)
	if err := l.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}

	for _, table := range owned {
		if !hasColumn(t, db, DialectSQLite, table, "owner") {
			t.Fatalf("Apply did not backfill the owner column on pre-existing %s", table)
		}
	}

	// The backfilled column must be usable with its default.
	id := uuid.NewString()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO loom_prompts (id, slug, version) VALUES ($1,'p',1)`, id); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := db.QueryRowContext(ctx,
		`SELECT owner FROM loom_prompts WHERE id=$1`, id).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "" {
		t.Fatalf("owner default = %q, want empty string", owner)
	}
}

// TestApplyPostgres runs the runner against Postgres when LOOM_DSN is set,
// covering the information_schema column-introspection path.
func TestApplyPostgres(t *testing.T) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("set LOOM_DSN to run the Postgres migration test")
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	l := NewLoader(DialectPostgres)
	if err := l.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ctx, db); err != nil {
		t.Fatalf("second Apply failed: %v", err)
	}

	applied, err := l.appliedVersions(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations() {
		if !applied[m.Version] {
			t.Fatalf("migration %d (%s) not recorded as applied", m.Version, m.Name)
		}
	}
	if !hasColumn(t, db, DialectPostgres, "loom_prompts", "owner") {
		t.Fatal("loom_prompts is missing the owner column after Apply")
	}
}

// TestMultiSchemaBackfillPostgres is the discriminating test for the schema-blind
// column check: a decoy loom_prompts in another schema already has owner, while
// the search-path target (public) does not. A schema-blind existence check would
// see the decoy, skip the ALTER, and leave public.loom_prompts permanently
// without owner. Apply must backfill the relation the ALTER actually targets.
func TestMultiSchemaBackfillPostgres(t *testing.T) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		t.Skip("set LOOM_DSN to run the Postgres multi-schema test")
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	l := NewLoader(DialectPostgres)
	if err := l.Apply(ctx, db); err != nil { // ensure the baseline tables exist
		t.Fatal(err)
	}

	t.Cleanup(func() {
		db.ExecContext(ctx, `DROP SCHEMA IF EXISTS mig_other CASCADE`)
	})

	// Recreate the discriminating state: public.loom_prompts without owner, no
	// ledger so migration 2 re-runs, and a decoy WITH owner in another schema
	// that is not on the search_path.
	setup := []string{
		`DROP TABLE IF EXISTS loom_schema_migrations`,
		`ALTER TABLE loom_prompts DROP COLUMN IF EXISTS owner`,
		`DROP SCHEMA IF EXISTS mig_other CASCADE`,
		`CREATE SCHEMA mig_other`,
		`CREATE TABLE mig_other.loom_prompts (id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '')`,
	}
	for _, s := range setup {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}

	if err := l.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}

	var found bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_attribute
			WHERE attrelid = 'public.loom_prompts'::regclass
			  AND attname = 'owner' AND attnum > 0 AND NOT attisdropped
		)`).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("public.loom_prompts was not backfilled: the decoy in mig_other masked the missing column")
	}
}
