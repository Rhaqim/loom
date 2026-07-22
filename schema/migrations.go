package schema

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// migrationsTable is the bare (un-prefixed) name of the ledger that records
// which migrations have run.
const migrationsTable = "schema_migrations"

// Migration is a single ordered, versioned change to the Loom schema. Version
// numbers are dense and start at 1. Up runs inside a transaction together with
// the ledger insert, so a failed migration rolls back cleanly and its version
// is never recorded.
type Migration struct {
	Version int
	Name    string
	Up      func(ctx context.Context, tx *sql.Tx, prefix string, d Dialect) error
}

// migrations is the ordered list of every schema migration. Append new
// migrations to the end with the next version number. Never edit or reorder an
// already-released migration — add a new one instead.
func migrations() []Migration {
	return []Migration{
		{
			Version: 1,
			Name:    "baseline",
			Up: func(ctx context.Context, tx *sql.Tx, p string, d Dialect) error {
				if d == DialectSQLite {
					return execAll(ctx, tx, sqliteStatements(p))
				}
				return execAll(ctx, tx, postgresStatements(p))
			},
		},
		{
			// The owner scope shipped after the baseline tables already existed
			// in some databases, where CREATE TABLE IF NOT EXISTS never adds the
			// new column. Add it where it is missing; a no-op on fresh DBs that
			// already got it from the baseline.
			Version: 2,
			Name:    "registry-owner-scope",
			Up: func(ctx context.Context, tx *sql.Tx, p string, d Dialect) error {
				for _, table := range []string{"prompts", "response_formats", "agents", "flows"} {
					if err := addColumnIfMissing(ctx, tx, d, p+table, "owner", "TEXT NOT NULL DEFAULT ''"); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			// Optimistic-concurrency version guards session updates against lost
			// writes. Backfill it on databases that predate the column.
			Version: 3,
			Name:    "session-version",
			Up: func(ctx context.Context, tx *sql.Tx, p string, d Dialect) error {
				return addColumnIfMissing(ctx, tx, d, p+"sessions", "version", "INT NOT NULL DEFAULT 0")
			},
		},
		{
			// The reusable-response-format feature added the response_formats table
			// and the agents.response_format_id / response_format columns. Databases
			// created before it never got them (CREATE TABLE IF NOT EXISTS does not
			// alter an existing agents table). Ensure the referenced table exists,
			// then backfill the columns — a no-op on fresh DBs that already have them.
			Version: 4,
			Name:    "agent-response-format",
			Up: func(ctx context.Context, tx *sql.Tx, p string, d Dialect) error {
				var createRF, idType, jsonType string
				if d == DialectSQLite {
					createRF = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sresponse_formats (
						id TEXT PRIMARY KEY,
						slug TEXT NOT NULL,
						owner TEXT NOT NULL DEFAULT '',
						version INTEGER NOT NULL,
						schema TEXT NOT NULL DEFAULT '{}',
						strict INTEGER NOT NULL DEFAULT 0,
						created_at DATETIME NOT NULL,
						UNIQUE (slug, version)
					)`, p)
					idType, jsonType = "TEXT", "TEXT"
				} else {
					createRF = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sresponse_formats (
						id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
						slug          TEXT NOT NULL,
						owner         TEXT NOT NULL DEFAULT '',
						version       INT NOT NULL,
						schema        JSONB NOT NULL DEFAULT '{}',
						strict        BOOL NOT NULL DEFAULT false,
						created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
						UNIQUE (slug, version)
					)`, p)
					idType, jsonType = "UUID", "JSONB"
				}
				if _, err := tx.ExecContext(ctx, createRF); err != nil {
					return err
				}
				fkDef := fmt.Sprintf("%s REFERENCES %sresponse_formats(id) ON DELETE RESTRICT", idType, p)
				if err := addColumnIfMissing(ctx, tx, d, p+"agents", "response_format_id", fkDef); err != nil {
					return err
				}
				return addColumnIfMissing(ctx, tx, d, p+"agents", "response_format", jsonType)
			},
		},
		{
			// FlowAgent carries RetryMode/MaxRetries at runtime, but the
			// flow_agents table never stored them, so a flow persisted with a
			// non-default retry policy came back with the engine default and the
			// write appeared to succeed. retry_mode stores the RetryMode enum,
			// whose zero value is RetryDiscard — so the DEFAULT 0 on existing
			// rows reproduces exactly what the old code returned.
			Version: 5,
			Name:    "flow-agent-retry-policy",
			Up: func(ctx context.Context, tx *sql.Tx, p string, d Dialect) error {
				if err := addColumnIfMissing(ctx, tx, d, p+"flow_agents", "retry_mode", "INT NOT NULL DEFAULT 0"); err != nil {
					return err
				}
				return addColumnIfMissing(ctx, tx, d, p+"flow_agents", "max_retries", "INT NOT NULL DEFAULT 0")
			},
		},
		{
			// The registry `owner` scope became a real lookup key: Get/List/
			// Delete on agents, prompts and response formats now filter by it,
			// so two owners can hold the same slug. The baseline tables keyed
			// uniqueness on (slug, version) alone, which forbids that, so the
			// constraint has to widen to include owner.
			//
			// IMPORTANT — this migration only widens the constraint on
			// Postgres. On SQLite a table-level UNIQUE is backed by a
			// sqlite_autoindex_* that cannot be dropped; removing it means
			// rebuilding the table, and PRAGMA foreign_keys cannot be toggled
			// inside a transaction — so a rebuild here would cascade-delete
			// dependent rows for anyone who opened their database with
			// foreign_keys(1). Refusing to do that silently, an EXISTING SQLite
			// database keeps the narrower (slug, version) key.
			//
			// That is safe, not a hole: reads and writes are owner-scoped either
			// way, so no tenant can see another's records. The only limitation
			// is that two owners cannot reuse a slug. Fresh databases get the
			// wide key from the baseline. To widen an existing SQLite database,
			// dump and reload it against the current baseline.
			Version: 6,
			Name:    "registry-owner-unique-key",
			Up: func(ctx context.Context, tx *sql.Tx, p string, d Dialect) error {
				if d == DialectSQLite {
					return nil
				}
				for _, c := range []struct {
					table  string
					oldKey string
					cols   []string
				}{
					{"prompts", "prompts_slug_version_kind_key", []string{"owner", "slug", "version", "kind"}},
					{"response_formats", "response_formats_slug_version_key", []string{"owner", "slug", "version"}},
					{"agents", "agents_slug_version_key", []string{"owner", "slug", "version"}},
				} {
					table := p + c.table

					// A fresh database already got the wide key inline from the
					// baseline. Creating a second, redundant unique index over
					// the same columns would be pure noise, so check first.
					has, err := hasUniqueOn(ctx, tx, table, c.cols)
					if err != nil {
						return err
					}
					if has {
						continue
					}

					// Widening the key can only fail on data that already
					// violates it — which happens when the narrow key was
					// dropped or never existed. Surface that as an actionable
					// message naming the offending rows, because the raw driver
					// error ("could not create unique index ... 23505") gives an
					// operator nothing to act on, and this failure blocks
					// Apply and therefore engine startup.
					if err := ensureNoDuplicates(ctx, tx, table, c.cols); err != nil {
						return err
					}

					// Create the wide key BEFORE dropping the narrow one, so the
					// table is never left unprotected — if anything below fails,
					// the whole migration rolls back with a key still in place.
					if _, err := tx.ExecContext(ctx, fmt.Sprintf(
						`CREATE UNIQUE INDEX %s_owner_key ON %s (%s)`,
						table, table, strings.Join(c.cols, ", "))); err != nil {
						return err
					}
					// Postgres names an inline UNIQUE constraint
					// <table>_<cols>_key, so the baseline's key is predictable.
					// IF EXISTS makes this a no-op when it was created under
					// another name or has already been removed.
					if _, err := tx.ExecContext(ctx, fmt.Sprintf(
						`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s%s`,
						table, p, c.oldKey)); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			// response_formats gained a category column, for parity with
			// agents, prompts and flows — it is what lets an authoring UI group
			// schemas in a picker. Nullable-with-default so existing rows are
			// unaffected and behave exactly as before ("" = ungrouped).
			Version: 7,
			Name:    "response-format-category",
			Up: func(ctx context.Context, tx *sql.Tx, p string, d Dialect) error {
				return addColumnIfMissing(ctx, tx, d, p+"response_formats", "category", "TEXT NOT NULL DEFAULT ''")
			},
		},
	}
}

// Apply brings db up to the latest schema version. It is safe to call on every
// startup: already-applied migrations are skipped via the ledger, and the
// baseline uses CREATE TABLE IF NOT EXISTS so adopting a pre-migration database
// is a no-op for tables that already exist.
//
// Migrations run sequentially; concurrent callers on a fresh database can race
// on the ledger insert (one fails with a duplicate version). Single-instance
// startup is unaffected; multi-instance coordination is a production concern.
func (l *Loader) Apply(ctx context.Context, db *sql.DB) error {
	if err := l.ensureLedger(ctx, db); err != nil {
		return fmt.Errorf("loom schema: ledger: %w", err)
	}
	applied, err := l.appliedVersions(ctx, db)
	if err != nil {
		return fmt.Errorf("loom schema: read ledger: %w", err)
	}
	for _, m := range migrations() {
		if applied[m.Version] {
			continue
		}
		if err := l.runMigration(ctx, db, m); err != nil {
			return fmt.Errorf("loom schema: migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

// ledgerTable returns the prefixed name of the migrations ledger.
func (l *Loader) ledgerTable() string { return l.prefix + migrationsTable }

func (l *Loader) ensureLedger(ctx context.Context, db *sql.DB) error {
	var stmt string
	if l.dialect == DialectSQLite {
		stmt = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL DEFAULT '',
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, l.ledgerTable())
	} else {
		stmt = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			version    INT PRIMARY KEY,
			name       TEXT NOT NULL DEFAULT '',
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, l.ledgerTable())
	}
	_, err := db.ExecContext(ctx, stmt)
	return err
}

func (l *Loader) appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT version FROM %s`, l.ledgerTable()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func (l *Loader) runMigration(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := m.Up(ctx, tx, l.prefix, l.dialect); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (version, name) VALUES ($1, $2)`, l.ledgerTable()),
		m.Version, m.Name); err != nil {
		return err
	}
	return tx.Commit()
}

// hasUniqueOn reports whether table already carries a unique index or
// constraint over exactly cols (order-independent). Postgres only — it reads
// the catalog directly, which is the reliable way to tell an inline UNIQUE
// from a separately created index.
func hasUniqueOn(ctx context.Context, tx *sql.Tx, table string, cols []string) (bool, error) {
	sorted := append([]string(nil), cols...)
	slices.Sort(sorted)
	want := strings.Join(sorted, ",")

	// string_agg rather than array_agg so the result scans into a plain string
	// and this file needs no Postgres driver types.
	rows, err := tx.QueryContext(ctx, `
		SELECT string_agg(a.attname, ',' ORDER BY a.attname)
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY (i.indkey)
		WHERE c.relname = $1 AND i.indisunique
		GROUP BY i.indexrelid`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var got string
		if err := rows.Scan(&got); err != nil {
			return false, err
		}
		if got == want {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ensureNoDuplicates returns an actionable error when table already holds rows
// that would violate a unique key over cols. It names the table, the key, and a
// sample of the conflicting values so an operator can dedupe without going
// spelunking — a migration failure here blocks Apply, and Apply gates engine
// startup.
func ensureNoDuplicates(ctx context.Context, tx *sql.Tx, table string, cols []string) error {
	list := strings.Join(cols, ", ")
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s, count(*) FROM %s GROUP BY %s HAVING count(*) > 1 LIMIT 5`,
		list, table, list))
	if err != nil {
		return err
	}
	defer rows.Close()

	var samples []string
	for rows.Next() {
		vals := make([]any, len(cols)+1)
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		parts := make([]string, len(cols))
		for i, col := range cols {
			parts[i] = fmt.Sprintf("%s=%v", col, derefBytes(vals[i]))
		}
		samples = append(samples, fmt.Sprintf("(%s) x%v", strings.Join(parts, ", "), derefBytes(vals[len(cols)])))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(samples) == 0 {
		return nil
	}
	return fmt.Errorf(
		"cannot add UNIQUE (%s) to %s: %d or more existing row groups already violate it "+
			"(e.g. %s). This table lost its narrower unique key at some point, so duplicates "+
			"were allowed in. Remove or re-key the duplicate rows, then re-run the migration; "+
			"loom will not delete them for you",
		list, table, len(samples), strings.Join(samples, "; "))
}

// derefBytes renders a scanned value, converting []byte (how the driver returns
// text columns) to a string so it prints readably instead of as a byte slice.
func derefBytes(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// execAll runs the statements in order on tx, annotating failures with the SQL.
func execAll(ctx context.Context, tx *sql.Tx, stmts []string) error {
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%w\nSQL: %s", err, s)
		}
	}
	return nil
}

// addColumnIfMissing adds column to table only when the catalog does not already
// report it, so the ALTER is safe to run on every dialect without relying on
// dialect-specific "IF NOT EXISTS" support for columns (SQLite has none).
func addColumnIfMissing(ctx context.Context, tx *sql.Tx, d Dialect, table, column, def string) error {
	exists, err := columnExists(ctx, tx, d, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, def))
	return err
}

// columnExists reports whether table has the named column, querying the
// dialect's catalog.
func columnExists(ctx context.Context, tx *sql.Tx, d Dialect, table, column string) (bool, error) {
	if d == DialectSQLite {
		// PRAGMA cannot be parameterized; table is an engine-controlled
		// prefix + fixed name, not user input.
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
		if err != nil {
			return false, err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				cid         int
				name, ctype string
				notnull, pk int
				dflt        sql.NullString
			)
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				return false, err
			}
			if name == column {
				return true, nil
			}
		}
		return false, rows.Err()
	}

	// Resolve the SAME relation the unqualified ALTER will hit: to_regclass
	// applies search_path and identifier folding exactly like ALTER TABLE, so
	// the check can't be fooled by a same-named table in another visible schema
	// (which information_schema.columns would match). NULL (no such table) makes
	// EXISTS false, so the ALTER still runs on a fresh or absent table.
	// Resolve the SAME relation the unqualified ALTER will hit: to_regclass
	// applies search_path and identifier folding exactly like ALTER TABLE, so
	// the check can't be fooled by a same-named table in another visible schema
	// (which information_schema.columns would match). NULL (no such table) makes
	// EXISTS false, so the ALTER still runs on a fresh or absent table.
	var found bool
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_attribute
			WHERE attrelid = to_regclass($1)
			  AND attname = $2
			  AND attnum > 0
			  AND NOT attisdropped
		)`, table, column).Scan(&found)
	return found, err
}
