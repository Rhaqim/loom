// Package anthropic provides a Loom generator adapter for Anthropic Claude models.
package anthropic

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

const (
	defaultBaseURL   = "https://api.anthropic.com/v1"
	anthropicVersion = "2023-06-01"
	// maxResponseBytes caps a full (non-streaming) response body so a malicious
	// or misbehaving upstream cannot exhaust memory with a huge reply. The
	// streaming path is bounded separately by the scanner's per-line limit.
	maxResponseBytes = 8 << 20 // 8 MiB
	// maxErrorBodyBytes truncates an upstream error body before it is echoed
	// into an error string (and thus logs).
	maxErrorBodyBytes = 4 << 10 // 4 KiB
)

// ChatGenerator implements loom.Generator and loom.StreamingGenerator for
// Anthropic Claude models (claude-opus-4, claude-sonnet-4, claude-haiku-4-5, etc.).
type ChatGenerator struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
	// allowedBaseURLs is the opt-in allowlist of base URLs a per-request
	// "base_url" override may select. Empty by default, which disables the
	// override: a caller-supplied base_url is otherwise a SSRF and
	// API-key-exfiltration primitive, since the request still carries this
	// generator's real credential.
	allowedBaseURLs map[string]struct{}
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

// WithBaseURL overrides the API base URL (useful for testing / proxies). The URL
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
// req.Overrides ("api_key", "model", "base_url"), so a single call can be routed
// to a different model or tenant key. Returns the receiver unchanged when no
// overrides apply. A "base_url" override is honored only when it passes
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
			return nil, fmt.Errorf("anthropic: base_url override %q is not permitted (allowlist it via WithAllowedBaseURLs)", v)
		}
		cp.baseURL = v
	}
	return &cp, nil
}

func (g *ChatGenerator) Modality() loom.Modality { return loom.ModalityText }

// Generate calls the Anthropic Messages API synchronously.
func (g *ChatGenerator) Generate(ctx context.Context, req loom.GenerateRequest) (loom.Result, error) {
	g, err := g.withOverrides(req)
	if err != nil {
		return nil, err
	}
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

		respBody, err := g.postRaw(ctx, "/messages", body)
		if err != nil {
			results <- loom.NewFailedResult(loom.ModalityText, err)
			return
		}
		defer respBody.Close()

		var assembled strings.Builder
		stop := "end_turn"
		var inTok, outTok int

		// Anthropic streams Server-Sent Events: "event: <type>" and "data: <json>"
		// lines separated by blanks. A bufio.Scanner over lines parses this; the
		// old json.Decoder over the raw body broke on the first non-JSON "event:"
		// line and silently returned empty content with fabricated zero usage.
		sc := bufio.NewScanner(respBody)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var event struct {
				Type    string `json:"type"`
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
				Delta struct {
					Text       string `json:"text"`
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(payload), &event) != nil {
				continue
			}
			switch event.Type {
			case "message_start":
				inTok = event.Message.Usage.InputTokens
			case "content_block_delta":
				if event.Delta.Text != "" {
					assembled.WriteString(event.Delta.Text)
					chunks <- loom.Chunk{Content: event.Delta.Text}
				}
			case "message_delta":
				if event.Usage.OutputTokens > 0 {
					outTok = event.Usage.OutputTokens
				}
				if event.Delta.StopReason != "" {
					stop = event.Delta.StopReason
				}
			}
		}
		if err := sc.Err(); err != nil && assembled.Len() == 0 {
			results <- loom.NewFailedResult(loom.ModalityText, err)
			return
		}
		tr := loom.NewTextResult(assembled.String(), stop, inTok, outTok)
		tr.Metadata()["model"] = g.model
		results <- tr
	}()

	return chunks, results, nil
}

// -----------------------------------------------------------------------
// Internal types and helpers
// -----------------------------------------------------------------------

type messagesRequest struct {
	Model       string         `json:"model"`
	MaxTokens   int            `json:"max_tokens"`
	Temperature float64        `json:"temperature,omitempty"`
	TopP        float64        `json:"top_p,omitempty"`
	System      string         `json:"system,omitempty"`
	Messages    []anthropicMsg `json:"messages"`
	Stream      bool           `json:"stream,omitempty"`
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
		Model:       g.model,
		MaxTokens:   maxTokens,
		Temperature: req.Params.Temperature,
		TopP:        req.Params.TopP,
		System:      systemWithFormat(req),
		Messages:    []anthropicMsg{{Role: "user", Content: req.UserPrompt}},
		Stream:      stream,
	}
	return json.Marshal(mr)
}

// systemWithFormat appends JSON-output guidance to the system prompt when the
// agent declares a response format. The Anthropic Messages API has no native
// response_format field, so the contract is steered through the system prompt
// and validated by loom's SchemaValidationPostHook; parseResponse then decodes
// the JSON into a StructuredResult.
func systemWithFormat(req loom.GenerateRequest) string {
	if req.ResponseFormat == nil {
		return req.SystemPrompt
	}
	var b strings.Builder
	b.WriteString(req.SystemPrompt)
	if req.SystemPrompt != "" {
		b.WriteString("\n\n")
	}
	b.WriteString("Respond with a single valid JSON object and nothing else.")
	if len(req.ResponseFormat.Schema) > 0 {
		if schemaJSON, err := json.Marshal(req.ResponseFormat.Schema); err == nil {
			b.WriteString(" It must conform to this JSON schema:\n")
			b.Write(schemaJSON)
		}
	}
	return b.String()
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
			sr := loom.NewStructuredResult(data, in, out)
			sr.Metadata()["model"] = g.model
			return sr, nil
		}
	}
	tr := loom.NewTextResult(content, mr.StopReason, in, out)
	tr.Metadata()["model"] = g.model
	return tr, nil
}

func (g *ChatGenerator) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	rc, err := g.postRaw(ctx, path, body)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxResponseBytes))
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
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, fmt.Errorf("anthropic: http %d: %s", resp.StatusCode, b)
	}
	return resp.Body, nil
}
