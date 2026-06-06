package game

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom-dnd/domain"
	"github.com/rhaqim/loom-dnd/store"
	"github.com/rhaqim/loom/generator/anthropic"
	"github.com/rhaqim/loom/generator/echo"
	"github.com/rhaqim/loom/generator/openai"
	"github.com/rhaqim/loom/schema"
)

const (
	loomSchemaPrefix = "loom_"
	dmUserPromptSlug = "dm-user-template"
	dmUserPromptVer  = 1
)

// Engine is the DND game engine — wraps loom.Engine and adds D&D orchestration.
type Engine struct {
	Loom  *loom.Engine
	Store *store.Store
}

// New creates a DND Engine from a *sql.DB.
// echo is always registered; openai/anthropic are added when API key env vars are set.
func New(ctx context.Context, db *sql.DB, st *store.Store) (*Engine, error) {
	cfg := loom.Config{
		DB:           db,
		Dialect:      loom.DialectPostgres,
		SchemaPrefix: loomSchemaPrefix,
		Generators:   AvailableGenerators(),
	}
	e, err := loom.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("loom engine: %w", err)
	}
	return &Engine{Loom: e, Store: st}, nil
}

// ApplyLoomSchema applies the loom schema (idempotent).
func ApplyLoomSchema(ctx context.Context, db *sql.DB) error {
	return schema.NewLoader(schema.DialectPostgres).Apply(ctx, db)
}

// AvailableGenerators returns the registered generators based on env vars.
func AvailableGenerators() map[string]loom.Generator {
	gens := map[string]loom.Generator{
		"echo": echo.New("[DM]"),
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		gens["openai"] = openai.NewChatGenerator(key, "gpt-4o")
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		gens["anthropic"] = anthropic.NewChatGenerator(key, "claude-opus-4-5")
	}
	return gens
}

// BestGeneratorSlug returns the slug of the best available generator.
// Priority: anthropic > openai > echo.
func BestGeneratorSlug() string {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "anthropic"
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		return "openai"
	}
	return "echo"
}

// SeedDMAgent ensures the loom prompts and agent for a topic exist (idempotent).
// It creates the system prompt, shared user template, and DM agent in loom,
// then returns the agent slug.
func (e *Engine) SeedDMAgent(ctx context.Context, topic *domain.Topic) (agentSlug string, err error) {
	agentSlug = "dm-" + topic.Slug
	sysSlug := "dm-sys-" + topic.Slug
	genSlug := BestGeneratorSlug()

	// ── System prompt ────────────────────────────────────────────────────
	sysPrompt := &loom.Prompt{
		Slug:     sysSlug,
		Version:  1,
		Kind:     loom.PromptKindSystem,
		Category: "dnd-dm",
		Body:     DMSystemPrompt(topic.Name, topic.Setting, topic.Edition),
		Notes:    "Auto-seeded by loom-dnd for topic: " + topic.Name,
	}
	if err := e.Loom.Prompts().Create(ctx, sysPrompt); err != nil && !isDuplicateErr(err) {
		return "", fmt.Errorf("seed system prompt: %w", err)
	}
	// Fetch the persisted system prompt to get its UUID.
	sp, err := e.Loom.Prompts().Get(ctx, sysSlug, 1)
	if err != nil {
		return "", fmt.Errorf("fetch system prompt: %w", err)
	}

	// ── User template ─────────────────────────────────────────────────────
	userPrompt := &loom.Prompt{
		Slug:     dmUserPromptSlug,
		Version:  dmUserPromptVer,
		Kind:     loom.PromptKindUserTemplate,
		Category: "dnd-dm",
		Body:     DMUserTemplate(),
		Notes:    "Shared DM user template — injects character state and player action",
	}
	if err := e.Loom.Prompts().Create(ctx, userPrompt); err != nil && !isDuplicateErr(err) {
		return "", fmt.Errorf("seed user template: %w", err)
	}
	// Fetch the persisted user template to get its UUID.
	ut, err := e.Loom.Prompts().Get(ctx, dmUserPromptSlug, dmUserPromptVer)
	if err != nil {
		return "", fmt.Errorf("fetch user template: %w", err)
	}

	// ── DM agent ──────────────────────────────────────────────────────────
	agent := &loom.Agent{
		Slug:           agentSlug,
		Version:        1,
		Category:       "dnd-dm",
		Modal:          loom.ModalityText,
		GeneratorSlug:  genSlug,
		SystemPromptID: sp.ID,
		UserTemplateID: ut.ID,
	}
	if err := e.Loom.Agents().Create(ctx, agent); err != nil && !isDuplicateErr(err) {
		return "", fmt.Errorf("seed agent: %w", err)
	}

	return agentSlug, nil
}

// StartSession loads or creates the loom Session for a campaign.
func (e *Engine) StartSession(ctx context.Context, campaign *domain.Campaign, character *domain.Character, platformID string) (*loom.Session, error) {
	if campaign.LoomSessionID != "" {
		sessID, err := uuid.Parse(campaign.LoomSessionID)
		if err != nil {
			return nil, fmt.Errorf("invalid session id %q: %w", campaign.LoomSessionID, err)
		}
		sess, err := e.Loom.Sessions().Get(ctx, sessID)
		if err != nil {
			return nil, fmt.Errorf("load session: %w", err)
		}
		// Refresh current character state in session vars.
		sess.State.Vars = character.VarsMap()
		return sess, nil
	}

	sess := &loom.Session{
		PlatformID: platformID,
		Tags:       []string{"dnd:true", "campaign:" + campaign.ID.String()},
		State: loom.State{
			Modality: loom.ModalityText,
			Vars:     character.VarsMap(),
		},
	}
	if err := e.Loom.Sessions().Create(ctx, sess); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	if err := e.Store.SetCampaignSession(ctx, campaign.ID, sess.ID.String()); err != nil {
		return nil, fmt.Errorf("link session to campaign: %w", err)
	}
	campaign.LoomSessionID = sess.ID.String()
	return sess, nil
}

// Play executes one player turn and returns the DM's response.
// Streams chunks to stdout if the generator supports streaming.
func (e *Engine) Play(ctx context.Context, sess *loom.Session, character *domain.Character, agentSlug, playerInput string) (string, error) {
	// Keep session vars in sync with the live character sheet.
	sess.State.Vars = character.VarsMap()

	var streamed string
	step, err := e.Loom.RunStep(ctx, sess, loom.StepRequest{
		AgentSlug: agentSlug,
		Action: &loom.Action{
			Kind:    loom.ActionFreeText,
			Payload: map[string]any{"text": playerInput},
		},
		OnChunk: func(chunk loom.Chunk) {
			fmt.Print(chunk.Content)
			streamed += chunk.Content
		},
	})
	if err != nil {
		return "", err
	}
	if streamed != "" {
		return streamed, nil
	}
	if tr, ok := step.Result.(*loom.TextResult); ok {
		return tr.Content, nil
	}
	return "", nil
}

// -----------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------

func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return containsStr(s, "duplicate key") || containsStr(s, "UNIQUE constraint failed")
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
