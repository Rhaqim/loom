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

// TestForeignKeysEnforcedSQLite proves the SQLite baseline gets cascade/restrict
// parity with Postgres when the connection opts into PRAGMA foreign_keys=ON.
func TestForeignKeysEnforcedSQLite(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite",
		"file:fk_"+uuid.NewString()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	if err := NewLoader(DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}

	var fk int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys pragma = %d, want 1 (DSN did not enable enforcement)", fk)
	}

	// A flow_agent referencing a non-existent flow must be rejected.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO loom_flow_agents (id, flow_id, position, agent_slug) VALUES ($1,$2,0,'a')`,
		uuid.NewString(), uuid.NewString()); err == nil {
		t.Fatal("expected a foreign-key violation inserting a flow_agent with a missing flow_id")
	}

	// Deleting a flow must cascade to its flow_agents.
	flowID := uuid.NewString()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO loom_flows (id, slug, version, created_at) VALUES ($1,'f',1,CURRENT_TIMESTAMP)`, flowID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO loom_flow_agents (id, flow_id, position, agent_slug) VALUES ($1,$2,0,'a')`,
		uuid.NewString(), flowID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM loom_flows WHERE id=$1`, flowID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM loom_flow_agents WHERE flow_id=$1`, flowID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected cascade delete to remove flow_agents, found %d", n)
	}
}

// TestAdoptSessionVersionSQLite is the prod-upgrade path for migration v3: a
// database whose sessions table predates the optimistic-concurrency version
// column. Apply must backfill it (default 0) so session CAS works after upgrade.
func TestAdoptSessionVersionSQLite(t *testing.T) {
	ctx := context.Background()
	db := openMemSQLite(t)

	if _, err := db.ExecContext(ctx, `CREATE TABLE loom_sessions (
		id         TEXT PRIMARY KEY,
		platform_id TEXT NOT NULL DEFAULT '',
		state      TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if hasColumn(t, db, DialectSQLite, "loom_sessions", "version") {
		t.Fatal("precondition: loom_sessions should not have version yet")
	}

	if err := NewLoader(DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if !hasColumn(t, db, DialectSQLite, "loom_sessions", "version") {
		t.Fatal("migration v3 did not backfill sessions.version")
	}

	id := uuid.NewString()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO loom_sessions (id, platform_id, state, created_at, updated_at) VALUES ($1,'p','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, id); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := db.QueryRowContext(ctx, `SELECT version FROM loom_sessions WHERE id=$1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("backfilled version default = %d, want 0", v)
	}
	// The optimistic-concurrency guard must work on the backfilled column.
	res, err := db.ExecContext(ctx, `UPDATE loom_sessions SET version=version+1 WHERE id=$1 AND version=0`, id)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("CAS update affected %d rows, want 1", n)
	}
}

// TestAdoptAgentResponseFormatSQLite reproduces the reported failure: an agents
// table created before the reusable-response-format feature lacks
// response_format_id, so inserts naming that column fail. Migration v4 must
// backfill it (and the response_format column) and ensure the referenced table
// exists.
func TestAdoptAgentResponseFormatSQLite(t *testing.T) {
	ctx := context.Background()
	db := openMemSQLite(t)

	// Pre-feature agents table: no response_format_id / response_format.
	if _, err := db.ExecContext(ctx, `CREATE TABLE loom_agents (
		id                TEXT PRIMARY KEY,
		slug              TEXT NOT NULL,
		owner             TEXT NOT NULL DEFAULT '',
		version           INTEGER NOT NULL,
		category          TEXT NOT NULL DEFAULT '',
		modality          TEXT NOT NULL,
		generator_slug    TEXT NOT NULL,
		system_prompt_id  TEXT,
		user_template_id  TEXT,
		params            TEXT NOT NULL DEFAULT '{}',
		fallback_agent_id TEXT,
		created_at        DATETIME NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if hasColumn(t, db, DialectSQLite, "loom_agents", "response_format_id") {
		t.Fatal("precondition: loom_agents should not have response_format_id yet")
	}

	if err := NewLoader(DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"response_format_id", "response_format"} {
		if !hasColumn(t, db, DialectSQLite, "loom_agents", col) {
			t.Fatalf("migration v4 did not backfill loom_agents.%s", col)
		}
	}
	// An insert naming response_format_id (the operation that was failing) works.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO loom_agents (id, slug, version, modality, generator_slug, response_format_id, created_at)
		 VALUES ($1,'dm',1,'text','g',NULL,CURRENT_TIMESTAMP)`, uuid.NewString()); err != nil {
		t.Fatalf("insert with response_format_id failed after backfill: %v", err)
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
