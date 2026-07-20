// Package game contains the story-generation logic that sits on top of loom.
package game

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/generator/anthropic"
	"github.com/rhaqim/loom/generator/echo"
	"github.com/rhaqim/loom/generator/openai"
	"github.com/rhaqim/loom/schema"
	"github.com/rhaqim/story-api/domain"
	"github.com/rhaqim/story-api/store"
)

const (
	loomPrefix      = "loom_"
	userPromptSlug  = "story-user-template"
	userPromptVer   = 1
	historyMaxChars = 6000 // rolling window kept in session.State.Vars["history"]
)

// PlayResult carries the outcome of a single turn.
type PlayResult struct {
	Response  string
	TurnIndex int
	Err       error
}

// Engine wraps loom.Engine and adds story-API orchestration.
type Engine struct {
	Loom  *loom.Engine
	Store *store.Store
}

// New creates a story Engine.
func New(ctx context.Context, db *sql.DB, st *store.Store) (*Engine, error) {
	cfg := loom.Config{
		DB:           db,
		Dialect:      loom.DialectPostgres,
		SchemaPrefix: loomPrefix,
		Generators:   availableGenerators(),
		Cache:        loom.NewInProcessCache(),
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

// BestGeneratorSlug returns the slug of the best available generator.
func BestGeneratorSlug() string {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "anthropic"
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		return "openai"
	}
	return "echo"
}

func availableGenerators() map[string]loom.Generator {
	gens := map[string]loom.Generator{
		"echo": echo.New("[Narrator]"),
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		gens["openai"] = openai.NewChatGenerator(key, "gpt-4o")
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		gens["anthropic"] = anthropic.NewChatGenerator(key, "claude-opus-4-5")
	}
	return gens
}

// SeedStoryAgent ensures the loom prompts and narrator agent for a topic exist.
// It is idempotent — safe to call on every request.
func (e *Engine) SeedStoryAgent(ctx context.Context, topic *domain.Topic) (agentSlug string, err error) {
	agentSlug = "narrator-" + topic.Slug
	sysSlug := "narrator-sys-" + topic.Slug

	// System prompt.
	sysPrompt := &loom.Prompt{
		Slug:     sysSlug,
		Version:  1,
		Kind:     loom.PromptKindSystem,
		Category: "story-api",
		Body:     SystemPrompt(topic.Name, topic.Genre),
		Notes:    "Auto-seeded for topic: " + topic.Name,
	}
	if err := e.Loom.Prompts().Create(ctx, sysPrompt); err != nil && !isDuplicateErr(err) {
		return "", fmt.Errorf("seed system prompt: %w", err)
	}
	sp, err := e.Loom.Prompts().Get(ctx, "", sysSlug, 1)
	if err != nil {
		return "", fmt.Errorf("fetch system prompt: %w", err)
	}

	// Shared user template.
	userPrompt := &loom.Prompt{
		Slug:     userPromptSlug,
		Version:  userPromptVer,
		Kind:     loom.PromptKindUserTemplate,
		Category: "story-api",
		Body:     UserTemplate(),
		Notes:    "Shared story-api user template — injects history and reader action",
	}
	if err := e.Loom.Prompts().Create(ctx, userPrompt); err != nil && !isDuplicateErr(err) {
		return "", fmt.Errorf("seed user template: %w", err)
	}
	ut, err := e.Loom.Prompts().Get(ctx, "", userPromptSlug, userPromptVer)
	if err != nil {
		return "", fmt.Errorf("fetch user template: %w", err)
	}

	// Narrator agent.
	agent := &loom.Agent{
		Slug:           agentSlug,
		Version:        1,
		Category:       "story-api",
		Modal:          loom.ModalityText,
		GeneratorSlug:  BestGeneratorSlug(),
		SystemPromptID: sp.ID,
		UserTemplateID: ut.ID,
	}
	if err := e.Loom.Agents().Create(ctx, agent); err != nil && !isDuplicateErr(err) {
		return "", fmt.Errorf("seed agent: %w", err)
	}

	return agentSlug, nil
}

// StartSession loads or creates the loom Session for a story.
func (e *Engine) StartSession(ctx context.Context, story *domain.Story, platformID string) (*loom.Session, error) {
	if story.LoomSessionID != "" {
		sessID, err := uuid.Parse(story.LoomSessionID)
		if err != nil {
			return nil, fmt.Errorf("invalid session id: %w", err)
		}
		sess, err := e.Loom.Sessions().Get(ctx, sessID)
		if err != nil {
			return nil, fmt.Errorf("load session: %w", err)
		}
		return sess, nil
	}

	sess := &loom.Session{
		PlatformID: platformID,
		Tags:       []string{"story-api:true", "story:" + story.ID.String()},
		State: loom.State{
			Modality: loom.ModalityText,
			Vars:     map[string]any{},
		},
	}
	if err := e.Loom.Sessions().Create(ctx, sess); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	story.LoomSessionID = sess.ID.String()
	if err := e.Store.UpdateStory(ctx, story); err != nil {
		return nil, fmt.Errorf("link session to story: %w", err)
	}
	return sess, nil
}

// Play executes one synchronous reader turn and returns the narrator's response.
func (e *Engine) Play(ctx context.Context, sess *loom.Session, agentSlug, readerInput string) (*PlayResult, error) {
	step, err := e.Loom.RunStep(ctx, sess, loom.StepRequest{
		AgentSlug: agentSlug,
		Action: &loom.Action{
			Kind:    loom.ActionFreeText,
			Payload: map[string]any{"text": readerInput},
		},
	})
	if err != nil {
		return nil, err
	}

	response := ""
	if tr, ok := step.Result.(*loom.TextResult); ok {
		response = tr.Content
	}

	// Append to rolling history so future turns have full context.
	appendHistory(sess, readerInput, response)
	_ = e.Loom.Sessions().Update(ctx, sess)

	return &PlayResult{Response: response, TurnIndex: step.Index}, nil
}

// PlayStream executes one reader turn with streaming.
// Chunks arrive on the first channel; the final PlayResult on the second.
// The caller must drain both channels.
func (e *Engine) PlayStream(ctx context.Context, sess *loom.Session, agentSlug, readerInput string) (<-chan loom.Chunk, <-chan *PlayResult) {
	chunks := make(chan loom.Chunk, 64)
	result := make(chan *PlayResult, 1)

	go func() {
		defer close(chunks)
		defer close(result)

		var streamed string
		step, err := e.Loom.RunStep(ctx, sess, loom.StepRequest{
			AgentSlug: agentSlug,
			Action: &loom.Action{
				Kind:    loom.ActionFreeText,
				Payload: map[string]any{"text": readerInput},
			},
			OnChunk: func(c loom.Chunk) {
				chunks <- c
				streamed += c.Content
			},
		})
		if err != nil {
			result <- &PlayResult{Err: err}
			return
		}

		response := streamed
		if response == "" {
			if tr, ok := step.Result.(*loom.TextResult); ok {
				response = tr.Content
			}
		}

		// Append to rolling history.
		appendHistory(sess, readerInput, response)
		_ = e.Loom.Sessions().Update(ctx, sess)

		result <- &PlayResult{Response: response, TurnIndex: step.Index}
	}()

	return chunks, result
}

// TurnsFromSession reconstructs Turn objects from a loom session's history.
func TurnsFromSession(sess *loom.Session) []domain.Turn {
	turns := make([]domain.Turn, 0, len(sess.History))
	for _, step := range sess.History {
		t := domain.Turn{
			Index:      step.Index,
			DurationMs: step.DurationMs,
			CreatedAt:  step.CreatedAt,
		}
		if step.Action != nil {
			if payload, ok := step.Action.Payload.(map[string]any); ok {
				if text, ok := payload["text"].(string); ok {
					t.PlayerInput = text
				}
			}
		}
		if tr, ok := step.Result.(*loom.TextResult); ok {
			t.DMResponse = tr.Content
		}
		turns = append(turns, t)
	}
	return turns
}

// SeedDefaultTopics ensures the built-in genre topics exist in the store and loom.
func SeedDefaultTopics(ctx context.Context, st *store.Store, e *Engine) error {
	defaults := []domain.Topic{
		{Slug: "fantasy", Name: "High Fantasy", Genre: "fantasy", Description: "Swords, sorcery, and ancient prophecies."},
		{Slug: "sci-fi", Name: "Science Fiction", Genre: "sci-fi", Description: "Stars, synthetics, and the long future of humanity."},
		{Slug: "horror", Name: "Horror", Genre: "horror", Description: "Things that lurk at the edge of reason."},
		{Slug: "mystery", Name: "Mystery", Genre: "mystery", Description: "Clues, motives, and a truth that won't stay buried."},
		{Slug: "thriller", Name: "Thriller", Genre: "thriller", Description: "Everything at stake, always one step behind."},
		{Slug: "adventure", Name: "Adventure", Genre: "adventure", Description: "Lost cities, impossible escapes, and the open horizon."},
	}
	for i := range defaults {
		t := &defaults[i]
		if existing, _ := st.GetTopicBySlug(ctx, t.Slug); existing != nil {
			continue
		}
		if err := st.CreateTopic(ctx, t); err != nil {
			return fmt.Errorf("seed topic %q: %w", t.Slug, err)
		}
		agentSlug, err := e.SeedStoryAgent(ctx, t)
		if err != nil {
			return fmt.Errorf("seed agent for %q: %w", t.Slug, err)
		}
		_ = st.UpdateTopicAgentSlug(ctx, t.ID, agentSlug)
	}
	return nil
}

// -----------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------

// appendHistory maintains a rolling text log of the conversation in
// session.State.Vars["history"], capped at historyMaxChars.
func appendHistory(sess *loom.Session, readerInput, response string) {
	existing := ""
	if h, ok := sess.State.Vars["history"].(string); ok {
		existing = h
	}
	entry := fmt.Sprintf("Reader: %s\nNarrator: %s\n\n", readerInput, response)
	combined := existing + entry
	if len(combined) > historyMaxChars {
		combined = combined[len(combined)-historyMaxChars:]
	}
	sess.State.Vars["history"] = combined
}

func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "duplicate key") || contains(s, "UNIQUE constraint failed")
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
