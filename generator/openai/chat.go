// Package openai provides a Loom generator adapter for OpenAI chat models.
package openai

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

const defaultBaseURL = "https://api.openai.com/v1"

// ChatGenerator implements loom.Generator and loom.StreamingGenerator for
// OpenAI chat-completion models (gpt-4o, gpt-4o-mini, o1, etc.).
type ChatGenerator struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// NewChatGenerator creates an OpenAI chat generator.
func NewChatGenerator(apiKey, model string) *ChatGenerator {
	return &ChatGenerator{
		apiKey:  apiKey,
		model:   model,
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

// WithBaseURL overrides the API base URL (useful for testing / Azure).
func (g *ChatGenerator) WithBaseURL(url string) *ChatGenerator {
	g.baseURL = url
	return g
}

func (g *ChatGenerator) Modality() loom.Modality { return loom.ModalityText }

// Generate calls the OpenAI Chat Completions API synchronously.
func (g *ChatGenerator) Generate(ctx context.Context, req loom.GenerateRequest) (loom.Result, error) {
	body, err := g.buildBody(req, false)
	if err != nil {
		return nil, err
	}
	resp, err := g.post(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	return g.parseResponse(resp, req)
}

// GenerateStream calls the OpenAI streaming API and delivers chunks on the
// first returned channel; the fully assembled Result arrives on the second.
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

		respBody, err := g.postStream(ctx, "/chat/completions", body)
		if err != nil {
			results <- newFailedResult(err)
			return
		}
		defer respBody.Close()

		var assembled strings.Builder
		dec := json.NewDecoder(respBody)

		for {
			// SSE lines look like: data: {...}\n
			// We decode the JSON payload directly.
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				break
			}
			var event struct {
				Choices []struct {
					Delta        struct{ Content string `json:"content"` } `json:"delta"`
					FinishReason string                                      `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal(raw, &event) == nil && len(event.Choices) > 0 {
				text := event.Choices[0].Delta.Content
				if text != "" {
					assembled.WriteString(text)
					chunks <- loom.Chunk{Content: text}
				}
			}
		}
		results <- loom.NewTextResult(assembled.String(), "stop", 0, 0)
	}()

	return chunks, results, nil
}

// -----------------------------------------------------------------------
// Internal types and helpers
// -----------------------------------------------------------------------

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	TopP           float64         `json:"top_p,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	ResponseFormat *respFormat     `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type respFormat struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      struct{ Content string `json:"content"` } `json:"message"`
		FinishReason string                                     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (g *ChatGenerator) buildBody(req loom.GenerateRequest, stream bool) ([]byte, error) {
	var msgs []chatMessage
	if req.SystemPrompt != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.SystemPrompt})
	}
	if req.UserPrompt != "" {
		msgs = append(msgs, chatMessage{Role: "user", Content: req.UserPrompt})
	}
	cr := chatRequest{
		Model:       g.model,
		Messages:    msgs,
		Temperature: req.Params.Temperature,
		MaxTokens:   req.Params.MaxTokens,
		TopP:        req.Params.TopP,
		Stream:      stream,
	}
	if req.ResponseFormat != nil {
		cr.ResponseFormat = &respFormat{
			Type:       "json_schema",
			JSONSchema: req.ResponseFormat.Schema,
		}
	}
	return json.Marshal(cr)
}

func (g *ChatGenerator) parseResponse(body []byte, req loom.GenerateRequest) (loom.Result, error) {
	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("openai: parse response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("openai: no choices in response")
	}
	content := cr.Choices[0].Message.Content
	finish := cr.Choices[0].FinishReason
	in, out := cr.Usage.PromptTokens, cr.Usage.CompletionTokens

	if req.ResponseFormat != nil {
		var data map[string]any
		if err := json.Unmarshal([]byte(content), &data); err == nil {
			return loom.NewStructuredResult(data, in, out), nil
		}
	}
	return loom.NewTextResult(content, finish, in, out), nil
}

func (g *ChatGenerator) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	rc, err := g.postStream(ctx, path, body)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (g *ChatGenerator) postStream(ctx context.Context, path string, body []byte) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: http: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai: http %d: %s", resp.StatusCode, b)
	}
	return resp.Body, nil
}

// newFailedResult returns a minimal failed result for goroutine error propagation.
func newFailedResult(err error) loom.Result {
	r := loom.NewTextResult("", "error", 0, 0)
	r.Metadata()["error"] = err.Error()
	return r
}
