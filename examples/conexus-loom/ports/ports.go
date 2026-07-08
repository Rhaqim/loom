// Package ports declares the seams between this engine and the larger Conexus
// monorepo. Each interface here is something the standalone build implements with
// a local adapter (see package adapters) and that you later swap for the real
// monorepo service — multimedia queue, memory vault, plugin registry, per-user AI
// preferences, telemetry — without touching the engine.
package ports

import (
	"context"

	"github.com/google/uuid"
	"github.com/rhaqim/conexus-loom/domain"
)

// Logger is the telemetry seam (monorepo: backend/pkg/telemetry).
type Logger interface {
	Info(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// Embedder turns text into a vector. The standalone memory vault can run without
// one (keyword recall); plug a real embedding service in for semantic recall.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// MemoryVault is the semantic memory seam (monorepo: app/conexus/memory.Vault).
// Store durable facts; Recall the most relevant for a query.
type MemoryVault interface {
	Store(ctx context.Context, storyID uuid.UUID, fact string) error
	Recall(ctx context.Context, storyID uuid.UUID, query string, topK int) ([]string, error)
}

// MediaKind enumerates async media outputs.
type MediaKind string

const (
	MediaImage   MediaKind = "image"
	MediaMusic   MediaKind = "music"
	MediaSFX     MediaKind = "sfx"
	MediaVoice   MediaKind = "voice"
	MediaAmbient MediaKind = "ambient"
	MediaVideo   MediaKind = "video"
	MediaTTS     MediaKind = "tts" // legacy alias for voice
)

// MediaTask is one queued async media job. Prompt is the primary text (image
// prompt, sfx cue, or the prose to voice); Params carries the channel-specific
// direction (mood, emotion, intensity, transition, …) for the provider behind
// the queue to consume.
type MediaTask struct {
	StoryID uuid.UUID
	TurnID  string
	Kind    MediaKind
	Prompt  string
	Params  map[string]any
}

// MultimediaQueue is the async media seam (monorepo: app/conexus/multimedia).
type MultimediaQueue interface {
	Enqueue(ctx context.Context, task MediaTask) error
}

// PlayerContext is passed to plugins for the acting player.
type PlayerContext struct {
	PlayerID      uuid.UUID
	CharacterName string
}

// PreProcessResult lets a plugin gate or rewrite an action before generation.
type PreProcessResult struct {
	Allow         bool
	Reason        string
	RewrittenText string
	InjectContext string
}

// Plugin is a game-mode extension (monorepo: app/conexus/narrative.Plugin).
// PreProcess/PostProcess and the prompt additions are how combat, multiplayer,
// etc. hook in without engine changes.
type Plugin interface {
	Name() string
	SystemPromptAddition() string
	ContextAddition(state domain.PlayState) string
	PreProcess(ctx context.Context, state *domain.PlayState, p PlayerContext, action string) (PreProcessResult, error)
	PostProcess(ctx context.Context, state *domain.PlayState, out *domain.LogicianOutput) error
}

// PluginRegistry resolves the active plugin for a story.
type PluginRegistry interface {
	For(storyID uuid.UUID) (Plugin, bool)
}

// ModelChoice is a per-agent provider/model override.
type ModelChoice struct {
	GeneratorSlug string // "" = use the agent's configured generator
	Model         string
}

// AIPreferences resolves per-account model overrides (monorepo: ai.AIPreferenceStore).
type AIPreferences interface {
	For(ctx context.Context, accountID string, agentSlug string) (ModelChoice, bool)
}

// TopicSource yields the raw block prompt for a story (monorepo: domain.TopicRepository).
type TopicSource interface {
	BlockPrompt(ctx context.Context, topicID string) (string, error)
}
