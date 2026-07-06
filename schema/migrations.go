package schema

import (
	"context"
	"database/sql"
	"fmt"
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
