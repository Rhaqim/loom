package db

var ddl = []string{
	`CREATE EXTENSION IF NOT EXISTS "pgcrypto"`,

	// story_users — registered players.
	`CREATE TABLE IF NOT EXISTS story_users (
		id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		username      TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,

	// story_topics — genre / setting definitions.
	`CREATE TABLE IF NOT EXISTS story_topics (
		id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		slug          TEXT NOT NULL UNIQUE,
		name          TEXT NOT NULL,
		description   TEXT NOT NULL DEFAULT '',
		genre         TEXT NOT NULL DEFAULT 'fantasy',
		dm_agent_slug TEXT NOT NULL DEFAULT '',
		created_by    UUID REFERENCES story_users(id) ON DELETE SET NULL,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,

	// story_sessions — an ongoing narrative for one user within one topic.
	`CREATE TABLE IF NOT EXISTS story_sessions (
		id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		user_id         UUID NOT NULL REFERENCES story_users(id) ON DELETE CASCADE,
		topic_id        UUID NOT NULL REFERENCES story_topics(id) ON DELETE CASCADE,
		loom_session_id TEXT NOT NULL DEFAULT '',
		name            TEXT NOT NULL,
		status          TEXT NOT NULL DEFAULT 'active',
		turn_count      INT  NOT NULL DEFAULT 0,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
}
