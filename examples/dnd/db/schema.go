package db

// ddl contains all idempotent DDL for the DND application layer.
// These tables are separate from loom's own schema (prefixed loom_).
var ddl = []string{
	// Enable UUID generation if not already available.
	`CREATE EXTENSION IF NOT EXISTS "pgcrypto"`,

	// dnd_topics — a D&D universe or setting (Forgotten Realms, Eberron, Homebrew, …)
	`CREATE TABLE IF NOT EXISTS dnd_topics (
		id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		slug        TEXT NOT NULL UNIQUE,
		name        TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		setting     TEXT NOT NULL DEFAULT 'high-fantasy',
		edition     TEXT NOT NULL DEFAULT '5e',
		dm_agent_slug TEXT NOT NULL DEFAULT '',
		created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,

	// dnd_users — human players.
	`CREATE TABLE IF NOT EXISTS dnd_users (
		id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		username   TEXT NOT NULL UNIQUE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,

	// dnd_campaigns — a story arc within a topic.
	`CREATE TABLE IF NOT EXISTS dnd_campaigns (
		id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		topic_id        UUID NOT NULL REFERENCES dnd_topics(id) ON DELETE CASCADE,
		loom_session_id TEXT,
		name            TEXT NOT NULL,
		description     TEXT NOT NULL DEFAULT '',
		status          TEXT NOT NULL DEFAULT 'active',
		created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,

	// dnd_characters — a player character in a campaign.
	`CREATE TABLE IF NOT EXISTS dnd_characters (
		id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		user_id       UUID NOT NULL REFERENCES dnd_users(id) ON DELETE CASCADE,
		campaign_id   UUID NOT NULL REFERENCES dnd_campaigns(id) ON DELETE CASCADE,
		name          TEXT NOT NULL,
		race          TEXT NOT NULL,
		class         TEXT NOT NULL,
		background    TEXT NOT NULL DEFAULT 'Folk Hero',
		level         INT  NOT NULL DEFAULT 1,
		experience    INT  NOT NULL DEFAULT 0,
		max_hp        INT  NOT NULL DEFAULT 10,
		current_hp    INT  NOT NULL DEFAULT 10,
		armor_class   INT  NOT NULL DEFAULT 10,
		strength      INT  NOT NULL DEFAULT 10,
		dexterity     INT  NOT NULL DEFAULT 10,
		constitution  INT  NOT NULL DEFAULT 10,
		intelligence  INT  NOT NULL DEFAULT 10,
		wisdom        INT  NOT NULL DEFAULT 10,
		charisma      INT  NOT NULL DEFAULT 10,
		gold          INT  NOT NULL DEFAULT 10,
		inventory     JSONB NOT NULL DEFAULT '[]',
		spell_slots   JSONB NOT NULL DEFAULT '{}',
		conditions    JSONB NOT NULL DEFAULT '[]',
		notes         TEXT  NOT NULL DEFAULT '',
		created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (campaign_id, user_id)
	)`,
}
