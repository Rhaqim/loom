// Package openai provides a Loom generator adapter for OpenAI chat models.
package openai

import (
	"bufio"
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

const (
	// maxResponseBytes caps a full (non-streaming) response body so a malicious
	// or misbehaving upstream cannot exhaust memory with a huge reply. The
	// streaming path is bounded separately by the scanner's per-line limit.
	maxResponseBytes = 8 << 20 // 8 MiB
	// maxErrorBodyBytes truncates an upstream error body before it is echoed
	// into an error string (and thus logs).
	maxErrorBodyBytes = 4 << 10 // 4 KiB
)

// ChatGenerator implements loom.Generator and loom.StreamingGenerator for
// OpenAI chat-completion models (gpt-4o, gpt-4o-mini, o1, etc.).
type ChatGenerator struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
	// allowedBaseURLs is the opt-in allowlist of base URLs a per-request
	// "base_url" override may select. It is empty by default, which disables
	// the override entirely — a per-request base_url is otherwise a SSRF and
	// API-key-exfiltration primitive, since the request still carries this
	// generator's real credential.
	allowedBaseURLs map[string]struct{}
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

// WithBaseURL overrides the API base URL (useful for testing / Azure). The URL
// set here is also implicitly trusted as a per-request "base_url" override.
func (g *ChatGenerator) WithBaseURL(url string) *ChatGenerator {
	g.baseURL = url
	return g
}

// WithAllowedBaseURLs opts into honoring the per-request "base_url" override,
// restricted to the given exact URLs. Anything not listed here (and not equal to
// the configured base URL) is rejected, so a caller-supplied base_url can never
// redirect the request — and this generator's API key — to an arbitrary host.
func (g *ChatGenerator) WithAllowedBaseURLs(urls ...string) *ChatGenerator {
	if g.allowedBaseURLs == nil {
		g.allowedBaseURLs = make(map[string]struct{}, len(urls))
	}
	for _, u := range urls {
		g.allowedBaseURLs[strings.TrimRight(u, "/")] = struct{}{}
	}
	return g
}

// baseURLAllowed reports whether v is a permitted per-request base URL. The
// configured base URL is always allowed; any other value must be explicitly
// allowlisted via WithAllowedBaseURLs. The allowlist is the operator's trust
// decision, so an attacker-supplied base_url that is neither the configured URL
// nor allowlisted is rejected — the API key is never sent to it.
func (g *ChatGenerator) baseURLAllowed(v string) bool {
	norm := strings.TrimRight(v, "/")
	if norm == strings.TrimRight(g.baseURL, "/") {
		return true
	}
	_, ok := g.allowedBaseURLs[norm]
	return ok
}

// withOverrides returns a per-request copy of the generator honoring
// req.Overrides ("api_key", "model", "base_url"), so callers can route a single
// call to a different model or tenant key. Returns the receiver unchanged when
// no overrides apply. A "base_url" override is honored only when it passes
// baseURLAllowed; otherwise withOverrides returns an error rather than fall back
// to the default endpoint (fail-closed). The shallow copy shares the
// (concurrency-safe) http.Client.
func (g *ChatGenerator) withOverrides(req loom.GenerateRequest) (*ChatGenerator, error) {
	if len(req.Overrides) == 0 {
		return g, nil
	}
	cp := *g
	if v, ok := req.Overrides["api_key"].(string); ok && v != "" {
		cp.apiKey = v
	}
	if v, ok := req.Overrides["model"].(string); ok && v != "" {
		cp.model = v
	}
	if v, ok := req.Overrides["base_url"].(string); ok && v != "" {
		if !g.baseURLAllowed(v) {
			return nil, fmt.Errorf("openai: base_url override %q is not permitted (allowlist it via WithAllowedBaseURLs)", v)
		}
		cp.baseURL = v
	}
	return &cp, nil
}

func (g *ChatGenerator) Modality() loom.Modality { return loom.ModalityText }

// Generate calls the OpenAI Chat Completions API synchronously.
func (g *ChatGenerator) Generate(ctx context.Context, req loom.GenerateRequest) (loom.Result, error) {
	g, err := g.withOverrides(req)
	if err != nil {
		return nil, err
	}
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
	g, err := g.withOverrides(req)
	if err != nil {
		return nil, nil, err
	}
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
		finish := "stop"
		var inTok, outTok int

		// OpenAI streams Server-Sent Events: each event is one or more
		// "data: <json>" lines terminated by a blank line, ending with
		// "data: [DONE]". A bufio.Scanner over lines parses this correctly;
		// json.Decoder cannot, because the "data: " prefix is not JSON.
		sc := bufio.NewScanner(respBody)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				break
			}
			var event struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(payload), &event) != nil {
				continue
			}
			if event.Usage != nil {
				inTok, outTok = event.Usage.PromptTokens, event.Usage.CompletionTokens
			}
			if len(event.Choices) > 0 {
				if fr := event.Choices[0].FinishReason; fr != "" {
					finish = fr
				}
				if text := event.Choices[0].Delta.Content; text != "" {
					assembled.WriteString(text)
					chunks <- loom.Chunk{Content: text}
				}
			}
		}
		if err := sc.Err(); err != nil && assembled.Len() == 0 {
			results <- newFailedResult(err)
			return
		}
		tr := loom.NewTextResult(assembled.String(), finish, inTok, outTok)
		tr.Metadata()["model"] = g.model
		results <- tr
	}()

	return chunks, results, nil
}

// -----------------------------------------------------------------------
// Internal types and helpers
// -----------------------------------------------------------------------

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	Temperature    float64        `json:"temperature,omitempty"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	TopP           float64        `json:"top_p,omitempty"`
	Stream         bool           `json:"stream,omitempty"`
	StreamOptions  *streamOptions `json:"stream_options,omitempty"`
	ResponseFormat *respFormat    `json:"response_format,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
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
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
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
	if stream {
		cr.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if req.ResponseFormat != nil {
		// Empty schema → portable JSON mode (works across most OpenAI-compatible
		// providers). A populated schema → strict structured outputs.
		if len(req.ResponseFormat.Schema) == 0 {
			cr.ResponseFormat = &respFormat{Type: "json_object"}
		} else {
			cr.ResponseFormat = &respFormat{
				Type: "json_schema",
				JSONSchema: map[string]any{
					"name":   "output",
					"schema": req.ResponseFormat.Schema,
					"strict": req.ResponseFormat.StrictMode,
				},
			}
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
			sr := loom.NewStructuredResult(data, in, out)
			sr.Metadata()["model"] = g.model
			return sr, nil
		}
	}
	tr := loom.NewTextResult(content, finish, in, out)
	tr.Metadata()["model"] = g.model
	return tr, nil
}

func (g *ChatGenerator) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	rc, err := g.postStream(ctx, path, body)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxResponseBytes))
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
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, fmt.Errorf("openai: http %d: %s", resp.StatusCode, b)
	}
	return resp.Body, nil
}

// newFailedResult returns a minimal failed result for goroutine error propagation.
func newFailedResult(err error) loom.Result {
	return loom.NewFailedResult(loom.ModalityText, err)
}
