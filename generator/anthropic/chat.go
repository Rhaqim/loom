// Package anthropic provides a Loom generator adapter for Anthropic Claude
// models, built on the official Anthropic Go SDK
// (github.com/anthropics/anthropic-sdk-go).
//
// The SDK speaks API version 2026-10-21. On that version a Messages request
// carrying temperature or top_p is rejected on every model, so this adapter
// never sends GenerateParams.Temperature or TopP: requests use the model's
// default sampling (Claude 4.7 and later models accept only the defaults
// anyway). The key is sent both as "Authorization: Bearer" (the SDK's default)
// and as "X-Api-Key", so proxies that read either header keep working.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	loom "github.com/rhaqim/loom"
)

// defaultBaseURL is the Claude API. A base URL may be given with or without
// the trailing /v1 (the SDK adds the version segment itself).
const defaultBaseURL = "https://api.anthropic.com/v1"

// defaultMaxTokens applies when GenerateParams.MaxTokens is unset. On current
// models thinking is on by default and its tokens count against max_tokens, so
// a low cap can be spent on reasoning before any visible text. 16000 keeps
// non-streaming requests within HTTP timeouts.
const defaultMaxTokens = 16000

// effortLevels are the accepted output_config.effort values.
var effortLevels = map[string]anthropic.OutputConfigEffort{
	"low":    anthropic.OutputConfigEffortLow,
	"medium": anthropic.OutputConfigEffortMedium,
	"high":   anthropic.OutputConfigEffortHigh,
	"xhigh":  anthropic.OutputConfigEffortXhigh,
	"max":    anthropic.OutputConfigEffortMax,
}

// ChatGenerator implements loom.Generator, loom.StreamingGenerator and
// loom.Verifier for Anthropic Claude models.
type ChatGenerator struct {
	apiKey  string
	model   string
	baseURL string
	client  anthropic.Client
	// allowedBaseURLs is the opt-in allowlist of base URLs a per-request
	// "base_url" override may select. Empty by default, which disables the
	// override: a caller-supplied base_url is otherwise a SSRF and
	// API-key-exfiltration primitive, since the request still carries this
	// generator's real credential.
	allowedBaseURLs map[string]struct{}
	// nativeFormat sends a ResponseFormat schema as native structured outputs
	// (output_config.format) instead of steering through the system prompt.
	// Opt-in: the API rejects schemas that don't meet its rules, which the
	// prompt-steered path accepts.
	nativeFormat bool
}

// NewChatGenerator creates an Anthropic chat generator for apiKey and model.
//
// The client ignores ANTHROPIC_* environment variables and saved login
// profiles: the key, model and base URL are exactly what the generator is
// configured with, so the environment cannot redirect requests or swap keys.
func NewChatGenerator(apiKey, model string) *ChatGenerator {
	return &ChatGenerator{
		apiKey:  apiKey,
		model:   model,
		baseURL: defaultBaseURL,
		client:  anthropic.NewClient(option.WithoutEnvironmentDefaults()),
	}
}

// WithBaseURL overrides the API base URL (useful for testing / proxies), with
// or without a trailing /v1. The URL set here is also implicitly trusted as a
// per-request "base_url" override.
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

// WithNativeStructuredOutputs makes a ResponseFormat with a schema use the
// Messages API's native structured outputs (output_config.format), which
// guarantees schema-valid JSON. Every object in the schema must set
// additionalProperties: false, and numeric or length constraints are not
// supported; a schema that breaks these rules is rejected by the API with a 400.
func (g *ChatGenerator) WithNativeStructuredOutputs() *ChatGenerator {
	g.nativeFormat = true
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
// to the default endpoint (fail-closed). The copy shares the SDK client, whose
// per-call settings come from requestOptions.
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

// requestOptions are the per-call SDK settings: this (possibly overridden)
// generator's base URL and key. The key goes in both auth headers (see the
// package doc).
func (g *ChatGenerator) requestOptions() []option.RequestOption {
	base := strings.TrimSuffix(strings.TrimRight(g.baseURL, "/"), "/v1") + "/"
	return []option.RequestOption{
		option.WithBaseURL(base),
		option.WithAPIKey(g.apiKey),
		option.WithHeader("X-Api-Key", g.apiKey),
	}
}

func (g *ChatGenerator) Modality() loom.Modality { return loom.ModalityText }

// Generate calls the Anthropic Messages API synchronously.
func (g *ChatGenerator) Generate(ctx context.Context, req loom.GenerateRequest) (loom.Result, error) {
	g, err := g.withOverrides(req)
	if err != nil {
		return nil, err
	}
	params, err := g.buildParams(req)
	if err != nil {
		return nil, err
	}
	msg, err := g.client.Messages.New(ctx, params, g.requestOptions()...)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	return g.toResult(msg, req)
}

// GenerateStream calls the Anthropic streaming API. Text deltas arrive on the
// first channel; the assembled Result (or a failed one) on the second.
func (g *ChatGenerator) GenerateStream(ctx context.Context, req loom.GenerateRequest) (<-chan loom.Chunk, <-chan loom.Result, error) {
	g, err := g.withOverrides(req)
	if err != nil {
		return nil, nil, err
	}
	params, err := g.buildParams(req)
	if err != nil {
		return nil, nil, err
	}

	chunks := make(chan loom.Chunk, 64)
	results := make(chan loom.Result, 1)

	go func() {
		defer close(chunks)
		defer close(results)

		stream := g.client.Messages.NewStreaming(ctx, params, g.requestOptions()...)
		defer stream.Close()
		var msg anthropic.Message
		var sawText bool
		for stream.Next() {
			ev := stream.Current()
			if err := msg.Accumulate(ev); err != nil {
				results <- loom.NewFailedResult(loom.ModalityText, fmt.Errorf("anthropic: stream: %w", err))
				return
			}
			if delta, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
				if text, ok := delta.Delta.AsAny().(anthropic.TextDelta); ok && text.Text != "" {
					sawText = true
					chunks <- loom.Chunk{Content: text.Text}
				}
			}
		}
		// A transport error before any text fails the result; after some text
		// the partial output is kept, as before.
		if err := stream.Err(); err != nil && !sawText {
			results <- loom.NewFailedResult(loom.ModalityText, fmt.Errorf("anthropic: %w", err))
			return
		}
		// A refusal, even mid-stream after some text, fails the result; the
		// engine surfaces it as GenerationRejected.
		if msg.StopReason == anthropic.StopReasonRefusal {
			results <- loom.NewFailedResult(loom.ModalityText, refusalError(msg.StopDetails))
			return
		}
		tr := loom.NewTextResult(messageText(&msg), string(msg.StopReason), int(msg.Usage.InputTokens), int(msg.Usage.OutputTokens))
		tr.Metadata()["model"] = g.model
		results <- tr
	}()

	return chunks, results, nil
}

// buildParams builds the Messages request.
//
//   - Params.Extra["effort"] ("low" | "medium" | "high" | "xhigh" | "max") is
//     sent as output_config.effort. Unset leaves the model's default, which on
//     Claude Opus 5.5 is "medium".
//   - With WithNativeStructuredOutputs, a ResponseFormat with a schema uses
//     native structured outputs (output_config.format). Otherwise the JSON
//     contract is steered through the system prompt.
//   - Temperature and TopP are never sent (see the package doc).
func (g *ChatGenerator) buildParams(req loom.GenerateRequest) (anthropic.MessageNewParams, error) {
	maxTokens := req.Params.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	params := anthropic.MessageNewParams{
		Model:     g.model,
		MaxTokens: int64(maxTokens),
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(req.UserPrompt))},
	}
	if v, ok := req.Params.Extra["effort"]; ok {
		name, _ := v.(string)
		effort, valid := effortLevels[name]
		if !valid {
			return params, fmt.Errorf("anthropic: effort %v is not one of low, medium, high, xhigh, max", v)
		}
		params.OutputConfig.Effort = effort
	}
	system := systemWithFormat(req)
	if rf := req.ResponseFormat; g.nativeFormat && rf != nil && len(rf.Schema) > 0 {
		params.OutputConfig.Format = anthropic.JSONOutputFormatParam{Schema: rf.Schema}
		system = req.SystemPrompt
	}
	if system != "" {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}
	return params, nil
}

// systemWithFormat appends JSON-output guidance to the system prompt when the
// agent declares a response format without native structured outputs. The
// contract is validated by loom's SchemaValidationPostHook; toResult then
// decodes the JSON into a StructuredResult.
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

// toResult converts a complete Message into a loom Result: a refusal becomes a
// GenerationRejected error, JSON output for a ResponseFormat a
// StructuredResult, anything else a TextResult.
func (g *ChatGenerator) toResult(msg *anthropic.Message, req loom.GenerateRequest) (loom.Result, error) {
	if msg.StopReason == anthropic.StopReasonRefusal {
		return nil, refusalError(msg.StopDetails)
	}
	content := messageText(msg)
	in, out := int(msg.Usage.InputTokens), int(msg.Usage.OutputTokens)
	if req.ResponseFormat != nil {
		var data map[string]any
		if err := json.Unmarshal([]byte(content), &data); err == nil {
			sr := loom.NewStructuredResult(data, in, out)
			sr.Metadata()["model"] = g.model
			return sr, nil
		}
	}
	tr := loom.NewTextResult(content, string(msg.StopReason), in, out)
	tr.Metadata()["model"] = g.model
	return tr, nil
}

// messageText joins a message's text blocks (thinking blocks are skipped).
func messageText(msg *anthropic.Message) string {
	var sb strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

// refusalError reports a safety refusal (stop_reason "refusal") as a typed
// rejection, so callers see GenerationRejected rather than an empty success.
func refusalError(d anthropic.RefusalStopDetails) error {
	msg := "anthropic: request refused"
	if d.Category != "" {
		msg += " (" + string(d.Category) + ")"
	}
	if d.Explanation != "" {
		msg += ": " + d.Explanation
	}
	return &loom.GenerationError{Kind: loom.GenerationRejected, Message: msg}
}

var _ loom.Verifier = (*ChatGenerator)(nil)

// Verify checks the API key and model against the Models API (GET
// /v1/models/{model}), honouring "api_key", "model" and "base_url" overrides.
// It generates nothing and spends no tokens. A 401/403 wraps
// loom.ErrKeyRejected; a 404 means the key works but the model does not exist.
func (g *ChatGenerator) Verify(ctx context.Context, overrides map[string]any) (loom.ModelInfo, error) {
	g, err := g.withOverrides(loom.GenerateRequest{Overrides: overrides})
	if err != nil {
		return loom.ModelInfo{}, err
	}
	m, err := g.client.Models.Get(ctx, g.model, anthropic.ModelGetParams{}, g.requestOptions()...)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				return loom.ModelInfo{}, fmt.Errorf("%w (anthropic http %d)", loom.ErrKeyRejected, apiErr.StatusCode)
			case http.StatusNotFound:
				return loom.ModelInfo{}, fmt.Errorf("anthropic: model %q not found for this key", g.model)
			}
		}
		return loom.ModelInfo{}, fmt.Errorf("anthropic: %w", err)
	}
	return loom.ModelInfo{
		ID:              m.ID,
		DisplayName:     m.DisplayName,
		ContextWindow:   int(m.MaxInputTokens),
		MaxOutputTokens: int(m.MaxTokens),
	}, nil
}
