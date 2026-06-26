// Package anthropic provides a Loom generator adapter for Anthropic Claude models.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	loom "github.com/rhaqim/loom"
)

const (
	defaultBaseURL   = "https://api.anthropic.com/v1"
	anthropicVersion = "2023-06-01"
)

// ChatGenerator implements loom.Generator and loom.StreamingGenerator for
// Anthropic Claude models (claude-opus-4, claude-sonnet-4, claude-haiku-4-5, etc.).
type ChatGenerator struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// NewChatGenerator creates an Anthropic chat generator.
func NewChatGenerator(apiKey, model string) *ChatGenerator {
	return &ChatGenerator{
		apiKey:  apiKey,
		model:   model,
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (g *ChatGenerator) Modality() loom.Modality { return loom.ModalityText }

// Generate calls the Anthropic Messages API synchronously.
func (g *ChatGenerator) Generate(ctx context.Context, req loom.GenerateRequest) (loom.Result, error) {
	body, err := g.buildBody(req, false)
	if err != nil {
		return nil, err
	}
	resp, err := g.post(ctx, "/messages", body)
	if err != nil {
		return nil, err
	}
	return g.parseResponse(resp, req)
}

// GenerateStream calls the Anthropic streaming API.
func (g *ChatGenerator) GenerateStream(ctx context.Context, req loom.GenerateRequest) (<-chan loom.Chunk, <-chan loom.Result, error) {
	body, err := g.buildBody(req, true)
	if err != nil {
		return nil, nil, err
	}

	chunks := make(chan loom.Chunk, 64)
	results := make(chan loom.Result, 1)

	go func() {
		defer close(chunks)
		defer close(results)

		respBody, err := g.postRaw(ctx, "/messages", body)
		if err != nil {
			results <- loom.NewFailedResult(loom.ModalityText, err)
			return
		}
		defer respBody.Close()

		var assembled strings.Builder
		dec := json.NewDecoder(respBody)

		for {
			var event struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := dec.Decode(&event); err != nil {
				break
			}
			if event.Type == "content_block_delta" && event.Delta.Text != "" {
				assembled.WriteString(event.Delta.Text)
				chunks <- loom.Chunk{Content: event.Delta.Text}
			}
		}
		results <- loom.NewTextResult(assembled.String(), "end_turn", 0, 0)
	}()

	return chunks, results, nil
}

// -----------------------------------------------------------------------
// Internal types and helpers
// -----------------------------------------------------------------------

type messagesRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system,omitempty"`
	Messages  []anthropicMsg `json:"messages"`
	Stream    bool           `json:"stream,omitempty"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (g *ChatGenerator) buildBody(req loom.GenerateRequest, stream bool) ([]byte, error) {
	maxTokens := req.Params.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	mr := messagesRequest{
		Model:     g.model,
		MaxTokens: maxTokens,
		System:    req.SystemPrompt,
		Messages:  []anthropicMsg{{Role: "user", Content: req.UserPrompt}},
		Stream:    stream,
	}
	return json.Marshal(mr)
}

func (g *ChatGenerator) parseResponse(body []byte, req loom.GenerateRequest) (loom.Result, error) {
	var mr messagesResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("anthropic: parse response: %w", err)
	}
	var sb strings.Builder
	for _, c := range mr.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	content := sb.String()
	in, out := mr.Usage.InputTokens, mr.Usage.OutputTokens

	if req.ResponseFormat != nil {
		var data map[string]any
		if err := json.Unmarshal([]byte(content), &data); err == nil {
			return loom.NewStructuredResult(data, in, out), nil
		}
	}
	return loom.NewTextResult(content, mr.StopReason, in, out), nil
}

func (g *ChatGenerator) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	rc, err := g.postRaw(ctx, path, body)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (g *ChatGenerator) postRaw(ctx context.Context, path string, body []byte) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", g.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: http: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic: http %d: %s", resp.StatusCode, b)
	}
	return resp.Body, nil
}
