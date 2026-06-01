// Package echo provides a deterministic stub generator for testing and examples.
// It echoes the user prompt back as a TextResult, making it useful for exercising
// the full engine pipeline (sessions, steps, hooks, cost tracking) without
// requiring a real LLM API key.
package echo

import (
	"context"
	"fmt"

	loom "github.com/rhaqim/loom"
)

// Generator implements loom.Generator and loom.StreamingGenerator.
// It always succeeds, returning the user prompt as the generated content.
type Generator struct {
	// Prefix is prepended to every response (optional).
	Prefix string
}

// New creates an echo generator with an optional response prefix.
func New(prefix string) *Generator {
	return &Generator{Prefix: prefix}
}

func (g *Generator) Modality() loom.Modality { return loom.ModalityText }

// Generate immediately returns the user prompt as a TextResult.
func (g *Generator) Generate(_ context.Context, req loom.GenerateRequest) (loom.Result, error) {
	content := req.UserPrompt
	if g.Prefix != "" {
		content = fmt.Sprintf("%s %s", g.Prefix, content)
	}
	return loom.NewTextResult(content, "stop", len(req.UserPrompt)/4, len(content)/4), nil
}

// GenerateStream streams the response word-by-word for demonstration purposes.
func (g *Generator) GenerateStream(_ context.Context, req loom.GenerateRequest) (<-chan loom.Chunk, <-chan loom.Result, error) {
	content := req.UserPrompt
	if g.Prefix != "" {
		content = fmt.Sprintf("%s %s", g.Prefix, content)
	}

	chunks := make(chan loom.Chunk, 8)
	results := make(chan loom.Result, 1)

	go func() {
		defer close(chunks)
		defer close(results)
		chunks <- loom.Chunk{Content: content}
		results <- loom.NewTextResult(content, "stop", len(req.UserPrompt)/4, len(content)/4)
	}()

	return chunks, results, nil
}
