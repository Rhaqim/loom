// Package adapters provides standalone, dependency-free implementations of every
// port so the engine runs and is testable before the real monorepo services are
// plugged in. Each type here is the thing you replace with a monorepo adapter.
package adapters

import (
	"bufio"
	"context"
	"log"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/rhaqim/conexus-refactor/domain"
	"github.com/rhaqim/conexus-refactor/ports"
	loom "github.com/rhaqim/loom"
)

// ---------------------------------------------------------------------------
// Logger
// ---------------------------------------------------------------------------

type StdLogger struct{ Verbose bool }

func (l StdLogger) Info(msg string, kv ...any) {
	if l.Verbose {
		log.Printf("INFO  %s %v", msg, kv)
	}
}
func (l StdLogger) Error(msg string, kv ...any) { log.Printf("ERROR %s %v", msg, kv) }

// ---------------------------------------------------------------------------
// MemoryVault — keyword recall, in-process. Swap for the pgvector vault.
// ---------------------------------------------------------------------------

type KeywordMemory struct {
	mu      sync.RWMutex
	byStory map[uuid.UUID][]string
}

func NewKeywordMemory() *KeywordMemory {
	return &KeywordMemory{byStory: map[uuid.UUID][]string{}}
}

func (m *KeywordMemory) Store(_ context.Context, storyID uuid.UUID, fact string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byStory[storyID] = append(m.byStory[storyID], fact)
	return nil
}

func (m *KeywordMemory) Recall(_ context.Context, storyID uuid.UUID, query string, topK int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	q := tokens(query)
	type scored struct {
		fact  string
		score int
	}
	var hits []scored
	for _, f := range m.byStory[storyID] {
		s := 0
		for w := range tokens(f) {
			if q[w] {
				s++
			}
		}
		if s > 0 {
			hits = append(hits, scored{f, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := []string{}
	for i := 0; i < len(hits) && i < topK; i++ {
		out = append(out, hits[i].fact)
	}
	return out, nil
}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,!?;:'\"()")
		if len(w) > 3 {
			out[w] = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// MultimediaQueue — logs enqueued jobs. Swap for the real worker queue.
// ---------------------------------------------------------------------------

type LogMultimedia struct{ Log ports.Logger }

func (q LogMultimedia) Enqueue(_ context.Context, t ports.MediaTask) error {
	if q.Log != nil {
		q.Log.Info("media enqueued", "kind", t.Kind, "turn", t.TurnID, "prompt", t.Prompt)
	}
	return nil
}

// ---------------------------------------------------------------------------
// PluginRegistry — empty by default. Register a Plugin per story to extend.
// ---------------------------------------------------------------------------

type PluginRegistry struct {
	mu      sync.RWMutex
	byStory map[uuid.UUID]ports.Plugin
}

func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{byStory: map[uuid.UUID]ports.Plugin{}}
}

func (r *PluginRegistry) Set(storyID uuid.UUID, p ports.Plugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byStory[storyID] = p
}

func (r *PluginRegistry) For(storyID uuid.UUID) (ports.Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byStory[storyID]
	return p, ok
}

// ---------------------------------------------------------------------------
// AIPreferences — no overrides by default.
// ---------------------------------------------------------------------------

type NoAIPreferences struct{}

func (NoAIPreferences) For(context.Context, string, string) (ports.ModelChoice, bool) {
	return ports.ModelChoice{}, false
}

// ---------------------------------------------------------------------------
// TopicSource — reads a block prompt from a file path (the topicID is the path).
// ---------------------------------------------------------------------------

type FileTopicSource struct{}

func (FileTopicSource) BlockPrompt(_ context.Context, topicID string) (string, error) {
	b, err := os.ReadFile(topicID)
	return string(b), err
}

// ---------------------------------------------------------------------------
// Config — minimal .env loader (no external dependency).
// ---------------------------------------------------------------------------

// LoadDotEnv reads KEY=VALUE lines from path into the process environment without
// overwriting variables already set. Missing file is not an error.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// Getenv returns the env value or a fallback.
func Getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// OpenAIPricing returns per-model USD prices (per 1M tokens) for common OpenAI
// models, for accurate cost tracking. Update the figures to match current rates;
// unknown models fall back to loom's DefaultPrice. Pass to loom.Config.Pricing.
func OpenAIPricing() map[string]loom.ModelPrice {
	return map[string]loom.ModelPrice{
		"gpt-4o-mini":  {InputPer1M: 0.15, OutputPer1M: 0.60},
		"gpt-4o":       {InputPer1M: 2.50, OutputPer1M: 10.00},
		"gpt-4.1":      {InputPer1M: 2.00, OutputPer1M: 8.00},
		"gpt-4.1-mini": {InputPer1M: 0.40, OutputPer1M: 1.60},
		"gpt-4.1-nano": {InputPer1M: 0.10, OutputPer1M: 0.40},
		"o4-mini":      {InputPer1M: 1.10, OutputPer1M: 4.40},
	}
}

// Compile-time interface checks.
var (
	_ ports.Logger          = StdLogger{}
	_ ports.MemoryVault     = (*KeywordMemory)(nil)
	_ ports.MultimediaQueue = LogMultimedia{}
	_ ports.PluginRegistry  = (*PluginRegistry)(nil)
	_ ports.AIPreferences   = NoAIPreferences{}
	_ ports.TopicSource     = FileTopicSource{}
	_                       = domain.World{}
)
