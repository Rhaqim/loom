// Package schema provides the versioned migration runner for the Loom engine.
// Apply migrations with:
//
//	loader := schema.NewLoader(schema.DialectPostgres)
//	err := loader.Apply(ctx, db)
//
// Migrations live in migrations.go as a dense, ordered list. Each runs exactly
// once and is recorded in the <prefix>schema_migrations ledger.
package schema

import (
	"fmt"
)

// Dialect identifies the database flavour.
type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectSQLite   Dialect = "sqlite"
)

// Loader applies the Loom DDL idempotently to a database.
type Loader struct {
	dialect Dialect
	prefix  string
}

// NewLoader creates a Loader for the given dialect.
// prefix is the table-name prefix (default: "loom_").
func NewLoader(dialect Dialect, prefix ...string) *Loader {
	p := "loom_"
	if len(prefix) > 0 && prefix[0] != "" {
		p = prefix[0]
	}
	return &Loader{dialect: dialect, prefix: p}
}

// postgresStatements returns Postgres DDL for all Loom tables.
func postgresStatements(p string) []string {
	return []string{
		// Extensions
		`CREATE EXTENSION IF NOT EXISTS "pgcrypto"`,

		// Prompts
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sprompts (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			slug          TEXT NOT NULL,
			owner         TEXT NOT NULL DEFAULT '',
			version       INT NOT NULL,
			kind          TEXT NOT NULL CHECK (kind IN ('system','user_template')),
			category      TEXT NOT NULL DEFAULT '',
			body          TEXT NOT NULL,
			variables     JSONB NOT NULL DEFAULT '[]',
			metadata      JSONB NOT NULL DEFAULT '{}',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
			notes         TEXT NOT NULL DEFAULT '',
			UNIQUE (slug, version, kind)
		)`, p),

		// Generators (configuration records)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sgenerators (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			slug          TEXT NOT NULL UNIQUE,
			modality      TEXT NOT NULL,
			provider      TEXT NOT NULL,
			config        JSONB NOT NULL DEFAULT '{}',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, p),

		// Response formats (reusable, versioned output contracts)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sresponse_formats (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			slug          TEXT NOT NULL,
			owner         TEXT NOT NULL DEFAULT '',
			version       INT NOT NULL,
			schema        JSONB NOT NULL DEFAULT '{}',
			strict        BOOL NOT NULL DEFAULT false,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (slug, version)
		)`, p),

		// Agents
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sagents (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			slug                TEXT NOT NULL,
			owner               TEXT NOT NULL DEFAULT '',
			version             INT NOT NULL,
			category            TEXT NOT NULL DEFAULT '',
			modality            TEXT NOT NULL,
			generator_slug      TEXT NOT NULL,
			system_prompt_id    UUID REFERENCES %sprompts(id) ON DELETE RESTRICT,
			user_template_id    UUID REFERENCES %sprompts(id) ON DELETE RESTRICT,
			response_format_id  UUID REFERENCES %sresponse_formats(id) ON DELETE RESTRICT,
			response_format     JSONB,
			params              JSONB NOT NULL DEFAULT '{}',
			fallback_agent_id   UUID,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (slug, version)
		)`, p, p, p, p),

		// Flows (persisted agent maps)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sflows (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			owner       TEXT NOT NULL DEFAULT '',
			slug        TEXT NOT NULL,
			version     INT NOT NULL,
			topic_id    TEXT,
			category    TEXT NOT NULL DEFAULT '',
			is_active   INT NOT NULL DEFAULT 1,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (owner, slug, version)
		)`, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sflow_agents (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			flow_id            UUID NOT NULL REFERENCES %sflows(id) ON DELETE CASCADE,
			position           INT NOT NULL,
			agent_slug         TEXT NOT NULL,
			agent_version      INT NOT NULL DEFAULT 0,
			output_key         TEXT NOT NULL DEFAULT '',
			stream             INT NOT NULL DEFAULT 0,
			generator_override TEXT NOT NULL DEFAULT '',
			params             JSONB NOT NULL DEFAULT '{}',
			UNIQUE (flow_id, position)
		)`, p, p),

		// Sessions
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %ssessions (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			platform_id         TEXT NOT NULL DEFAULT '',
			parent_session_id   UUID REFERENCES %ssessions(id) ON DELETE SET NULL,
			branch_point        INT,
			state               JSONB NOT NULL DEFAULT '{}',
			metadata            JSONB NOT NULL DEFAULT '{}',
			tags                JSONB NOT NULL DEFAULT '[]',
			pinned              BOOL NOT NULL DEFAULT false,
			deleted_at          TIMESTAMPTZ,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, p, p),

		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %ssessions_platform_id_idx ON %ssessions (platform_id)`, p, p),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %ssessions_parent_idx ON %ssessions (parent_session_id)`, p, p),

		// Tasks (async provider jobs)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %stasks (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			provider    TEXT NOT NULL,
			handle      TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'pending',
			attempts    INT NOT NULL DEFAULT 0,
			last_error  TEXT NOT NULL DEFAULT '',
			result_id   UUID,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, p),

		// Results
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sresults (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			modality    TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'ready',
			payload     JSONB NOT NULL DEFAULT '{}',
			task_id     UUID REFERENCES %stasks(id) ON DELETE SET NULL,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, p, p),

		// Actions
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sactions (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			session_id  UUID NOT NULL REFERENCES %ssessions(id) ON DELETE CASCADE,
			step_index  INT NOT NULL,
			kind        TEXT NOT NULL,
			payload     JSONB NOT NULL DEFAULT '{}',
			metadata    JSONB NOT NULL DEFAULT '{}',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, p, p),

		// Steps
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %ssteps (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			session_id   UUID NOT NULL REFERENCES %ssessions(id) ON DELETE CASCADE,
			step_index   INT NOT NULL,
			agent_id     UUID NOT NULL REFERENCES %sagents(id) ON DELETE RESTRICT,
			request      JSONB NOT NULL DEFAULT '{}',
			result_id    UUID NOT NULL REFERENCES %sresults(id) ON DELETE RESTRICT,
			action_id    UUID REFERENCES %sactions(id) ON DELETE SET NULL,
			annotations  JSONB NOT NULL DEFAULT '[]',
			diagnostics  JSONB NOT NULL DEFAULT '{}',
			duration_ms  INT NOT NULL DEFAULT 0,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (session_id, step_index)
		)`, p, p, p, p, p),

		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %ssteps_session_idx ON %ssteps (session_id, step_index)`, p, p),

		// Action templates (current available actions for a session)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %saction_templates (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			session_id  UUID NOT NULL REFERENCES %ssessions(id) ON DELETE CASCADE,
			kind        TEXT NOT NULL,
			label       TEXT NOT NULL DEFAULT '',
			detail      TEXT NOT NULL DEFAULT '',
			payload     JSONB,
			constraints JSONB,
			ordering    INT NOT NULL DEFAULT 0,
			active      BOOL NOT NULL DEFAULT true,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, p, p),

		// State snapshots (checkpoints)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sstate_snapshots (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			session_id  UUID NOT NULL REFERENCES %ssessions(id) ON DELETE CASCADE,
			step_index  INT NOT NULL,
			snapshot    BYTEA,
			vars        JSONB NOT NULL DEFAULT '{}',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, p, p),

		// Cost records (partitioned by month in production)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %scost_records (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			step_id       UUID REFERENCES %ssteps(id) ON DELETE SET NULL,
			session_id    UUID REFERENCES %ssessions(id) ON DELETE SET NULL,
			agent_id      UUID REFERENCES %sagents(id) ON DELETE SET NULL,
			platform_id   TEXT NOT NULL DEFAULT '',
			provider      TEXT NOT NULL DEFAULT '',
			model         TEXT NOT NULL DEFAULT '',
			modality      TEXT NOT NULL DEFAULT '',
			input_tokens  INT NOT NULL DEFAULT 0,
			output_tokens INT NOT NULL DEFAULT 0,
			images        INT NOT NULL DEFAULT 0,
			duration_sec  REAL NOT NULL DEFAULT 0,
			usd_cost      NUMERIC(10,6) NOT NULL DEFAULT 0,
			estimated     BOOL NOT NULL DEFAULT false,
			tags          JSONB NOT NULL DEFAULT '[]',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, p, p, p, p),

		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %scost_session_idx ON %scost_records (session_id, created_at)`, p, p),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %scost_platform_idx ON %scost_records (platform_id, created_at)`, p, p),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %scost_agent_idx ON %scost_records (agent_id, created_at)`, p, p),

		// Budgets
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sbudgets (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name          TEXT NOT NULL,
			target_kind   TEXT NOT NULL,
			target_key    TEXT NOT NULL,
			"window"      TEXT NOT NULL,
			limit_usd     NUMERIC(10,4),
			limit_tokens  BIGINT,
			limit_steps   INT,
			on_exceed     TEXT NOT NULL DEFAULT 'block',
			tags_include  JSONB NOT NULL DEFAULT '[]',
			tags_exclude  JSONB NOT NULL DEFAULT '[]',
			active        BOOL NOT NULL DEFAULT true,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (target_kind, target_key, name)
		)`, p),

		// Pricing tables
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %spricing_tables (
			provider                      TEXT NOT NULL,
			model                         TEXT NOT NULL,
			modality                      TEXT NOT NULL,
			rate_input_per_1m_tokens      NUMERIC(10,6),
			rate_output_per_1m_tokens     NUMERIC(10,6),
			rate_per_image                NUMERIC(10,6),
			rate_per_second               NUMERIC(10,6),
			effective_from                TIMESTAMPTZ NOT NULL DEFAULT now(),
			effective_to                  TIMESTAMPTZ,
			source                        TEXT NOT NULL DEFAULT 'engine-default',
			PRIMARY KEY (provider, model, effective_from)
		)`, p),
	}
}

// sqliteStatements returns SQLite-compatible DDL (for testing and embedded use).
// The foreign keys below mirror the Postgres constraints, but SQLite only
// enforces them when the connection sets PRAGMA foreign_keys=ON — open the DB
// with "...?_pragma=foreign_keys(1)" to get cascade/restrict parity. Without it
// the constraints are declared but inert (the historical default), so enabling
// enforcement is opt-in and does not change behavior for existing callers.
// Caveat: SQLite cannot add a foreign key to an existing table, so these
// constraints only apply to databases created fresh from this baseline; a
// pre-existing FK-less table is not retrofitted.
func sqliteStatements(p string) []string {
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sprompts (
			id TEXT PRIMARY KEY,
			slug TEXT NOT NULL,
			owner TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL,
			kind TEXT NOT NULL,
			category TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL,
			variables TEXT NOT NULL DEFAULT '[]',
			metadata TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL,
			notes TEXT NOT NULL DEFAULT '',
			UNIQUE (slug, version, kind)
		)`, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sgenerators (
			id TEXT PRIMARY KEY,
			slug TEXT NOT NULL UNIQUE,
			modality TEXT NOT NULL,
			provider TEXT NOT NULL,
			config TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL
		)`, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sresponse_formats (
			id TEXT PRIMARY KEY,
			slug TEXT NOT NULL,
			owner TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL,
			schema TEXT NOT NULL DEFAULT '{}',
			strict INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			UNIQUE (slug, version)
		)`, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sagents (
			id TEXT PRIMARY KEY,
			slug TEXT NOT NULL,
			owner TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL,
			category TEXT NOT NULL DEFAULT '',
			modality TEXT NOT NULL,
			generator_slug TEXT NOT NULL,
			system_prompt_id TEXT REFERENCES %sprompts(id) ON DELETE RESTRICT,
			user_template_id TEXT REFERENCES %sprompts(id) ON DELETE RESTRICT,
			response_format_id TEXT REFERENCES %sresponse_formats(id) ON DELETE RESTRICT,
			response_format TEXT,
			params TEXT NOT NULL DEFAULT '{}',
			fallback_agent_id TEXT,
			created_at DATETIME NOT NULL,
			UNIQUE (slug, version)
		)`, p, p, p, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sflows (
			id          TEXT PRIMARY KEY,
			owner       TEXT NOT NULL DEFAULT '',
			slug        TEXT NOT NULL,
			version     INTEGER NOT NULL,
			topic_id    TEXT,
			category    TEXT NOT NULL DEFAULT '',
			is_active   INTEGER NOT NULL DEFAULT 1,
			created_at  DATETIME NOT NULL,
			UNIQUE (owner, slug, version)
		)`, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sflow_agents (
			id                 TEXT PRIMARY KEY,
			flow_id            TEXT NOT NULL REFERENCES %sflows(id) ON DELETE CASCADE,
			position           INTEGER NOT NULL,
			agent_slug         TEXT NOT NULL,
			agent_version      INTEGER NOT NULL DEFAULT 0,
			output_key         TEXT NOT NULL DEFAULT '',
			stream             INTEGER NOT NULL DEFAULT 0,
			generator_override TEXT NOT NULL DEFAULT '',
			params             TEXT NOT NULL DEFAULT '{}',
			UNIQUE (flow_id, position)
		)`, p, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %ssessions (
			id TEXT PRIMARY KEY,
			platform_id TEXT NOT NULL DEFAULT '',
			parent_session_id TEXT REFERENCES %ssessions(id) ON DELETE SET NULL,
			branch_point INTEGER,
			state TEXT NOT NULL DEFAULT '{}',
			metadata TEXT NOT NULL DEFAULT '{}',
			tags TEXT NOT NULL DEFAULT '[]',
			pinned INTEGER NOT NULL DEFAULT 0,
			deleted_at DATETIME,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`, p, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %stasks (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			handle TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			result_id TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sresults (
			id TEXT PRIMARY KEY,
			modality TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'ready',
			payload TEXT NOT NULL DEFAULT '{}',
			task_id TEXT REFERENCES %stasks(id) ON DELETE SET NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`, p, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sactions (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES %ssessions(id) ON DELETE CASCADE,
			step_index INTEGER NOT NULL,
			kind TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '{}',
			metadata TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL
		)`, p, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %ssteps (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES %ssessions(id) ON DELETE CASCADE,
			step_index INTEGER NOT NULL,
			agent_id TEXT NOT NULL REFERENCES %sagents(id) ON DELETE RESTRICT,
			request TEXT NOT NULL DEFAULT '{}',
			result_id TEXT NOT NULL REFERENCES %sresults(id) ON DELETE RESTRICT,
			action_id TEXT REFERENCES %sactions(id) ON DELETE SET NULL,
			annotations TEXT NOT NULL DEFAULT '[]',
			diagnostics TEXT NOT NULL DEFAULT '{}',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			UNIQUE (session_id, step_index)
		)`, p, p, p, p, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %saction_templates (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES %ssessions(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			payload TEXT,
			constraints TEXT,
			ordering INTEGER NOT NULL DEFAULT 0,
			active INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL
		)`, p, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sstate_snapshots (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES %ssessions(id) ON DELETE CASCADE,
			step_index INTEGER NOT NULL,
			snapshot BLOB,
			vars TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL
		)`, p, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %scost_records (
			id TEXT PRIMARY KEY,
			step_id TEXT REFERENCES %ssteps(id) ON DELETE SET NULL,
			session_id TEXT REFERENCES %ssessions(id) ON DELETE SET NULL,
			agent_id TEXT REFERENCES %sagents(id) ON DELETE SET NULL,
			platform_id TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			modality TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			images INTEGER NOT NULL DEFAULT 0,
			duration_sec REAL NOT NULL DEFAULT 0,
			usd_cost REAL NOT NULL DEFAULT 0,
			estimated INTEGER NOT NULL DEFAULT 0,
			tags TEXT NOT NULL DEFAULT '[]',
			created_at DATETIME NOT NULL
		)`, p, p, p, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %sbudgets (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			target_kind TEXT NOT NULL,
			target_key TEXT NOT NULL,
			"window" TEXT NOT NULL,
			limit_usd REAL,
			limit_tokens INTEGER,
			limit_steps INTEGER,
			on_exceed TEXT NOT NULL DEFAULT 'block',
			tags_include TEXT NOT NULL DEFAULT '[]',
			tags_exclude TEXT NOT NULL DEFAULT '[]',
			active INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL,
			UNIQUE (target_kind, target_key, name)
		)`, p),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %spricing_tables (
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			modality TEXT NOT NULL,
			rate_input_per_1m_tokens REAL,
			rate_output_per_1m_tokens REAL,
			rate_per_image REAL,
			rate_per_second REAL,
			effective_from DATETIME NOT NULL,
			effective_to DATETIME,
			source TEXT NOT NULL DEFAULT 'engine-default',
			PRIMARY KEY (provider, model, effective_from)
		)`, p),
	}
}
