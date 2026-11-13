package anthropic

// chat_test.go — hermetic coverage for the SDK-based Anthropic adapter. Canned
// Messages API responses (JSON and complete SSE streams, as the real API sends
// them) are served by httptest; nothing reaches the network.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	loom "github.com/rhaqim/loom"
)

// seen is what a fake server observed about the last request.
type seen struct {
	path, bearer, xAPIKey string
	body                  map[string]any
}

func record(r *http.Request, s *seen) {
	s.path = r.URL.Path
	s.bearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.xAPIKey = r.Header.Get("X-Api-Key")
	b, _ := io.ReadAll(r.Body)
	s.body = nil
	_ = json.Unmarshal(b, &s.body)
}

// jsonServer replies to every request with status and body as JSON.
func jsonServer(t *testing.T, status int, body string, s *seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s != nil {
			record(r, s)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// message is a complete Messages API response body.
func message(text, stop string, in, out int, extra string) string {
	content := `[]`
	if text != "" {
		b, _ := json.Marshal(text)
		content = fmt.Sprintf(`[{"type":"text","text":%s}]`, b)
	}
	return fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":%s,"stop_reason":%q,"stop_sequence":null%s,"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		content, stop, extra, in, out)
}

// sseStream is a complete SSE stream: message_start, one text block streamed
// in parts, then message_delta carrying the stop reason (and delta extras).
func sseStream(in, out int, stop, deltaExtra string, parts ...string) string {
	var b strings.Builder
	ev := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":0}}}`, in))
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	for _, p := range parts {
		t, _ := json.Marshal(p)
		ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, t))
	}
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null%s},"usage":{"output_tokens":%d}}`, stop, deltaExtra, out))
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

func sseServer(t *testing.T, stream string, s *seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s != nil {
			record(r, s)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func drain(chunks <-chan loom.Chunk, results <-chan loom.Result) (string, loom.Result) {
	var b strings.Builder
	for c := range chunks {
		b.WriteString(c.Content)
	}
	return b.String(), <-results
}

func systemText(body map[string]any) string {
	blocks, _ := body["system"].([]any)
	var b strings.Builder
	for _, blk := range blocks {
		m, _ := blk.(map[string]any)
		s, _ := m["text"].(string)
		b.WriteString(s)
	}
	return b.String()
}

func TestStreamParsesSSEAndUsage(t *testing.T) {
	var s seen
	srv := sseServer(t, sseStream(12, 5, "end_turn", "", "Hello ", "world"), &s)
	chunks, results, err := NewChatGenerator("k", "claude-test").WithBaseURL(srv.URL).GenerateStream(context.Background(), loom.GenerateRequest{UserPrompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	text, res := drain(chunks, results)
	if text != "Hello world" {
		t.Errorf("streamed chunks = %q", text)
	}
	tr, ok := res.(*loom.TextResult)
	if !ok {
		t.Fatalf("result type %T (%v), want *loom.TextResult", res, res.Metadata()["error"])
	}
	if tr.Content != "Hello world" || tr.InputTokens != 12 || tr.OutputTokens != 5 || tr.FinishReason != "end_turn" {
		t.Errorf("result = %+v", tr)
	}
	if res.Metadata()["model"] != "claude-test" {
		t.Errorf("model metadata = %v", res.Metadata()["model"])
	}
	if s.path != "/v1/messages" || s.body["stream"] != true {
		t.Errorf("request path %q stream %v", s.path, s.body["stream"])
	}
}

// Sampling parameters are never sent (API version 2026-10-21 rejects them),
// the key goes in both auth headers, and the schema is steered through the
// system prompt by default.
func TestGenerateRequestShape(t *testing.T) {
	var s seen
	srv := jsonServer(t, 200, message(`{"answer":42}`, "end_turn", 3, 4, ""), &s)
	res, err := NewChatGenerator("secret", "claude-test").WithBaseURL(srv.URL).Generate(context.Background(), loom.GenerateRequest{
		SystemPrompt:   "be helpful",
		UserPrompt:     "q",
		ResponseFormat: &loom.ResponseFormat{Schema: map[string]any{"type": "object"}, StrictMode: true},
		Params:         loom.GenerateParams{Temperature: 0.4, TopP: 0.9, MaxTokens: 777},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"temperature", "top_p", "top_k"} {
		if _, ok := s.body[k]; ok {
			t.Errorf("%s was sent; API version 2026-10-21 rejects it", k)
		}
	}
	if s.body["max_tokens"] != float64(777) || s.body["model"] != "claude-test" {
		t.Errorf("max_tokens %v model %v", s.body["max_tokens"], s.body["model"])
	}
	if s.bearer != "secret" || s.xAPIKey != "secret" {
		t.Errorf("auth headers: bearer %q x-api-key %q, want the key in both", s.bearer, s.xAPIKey)
	}
	if sys := systemText(s.body); !strings.Contains(sys, "be helpful") || !strings.Contains(sys, `"type":"object"`) {
		t.Errorf("system prompt missing schema injection: %q", sys)
	}
	if sr, ok := res.(*loom.StructuredResult); !ok || sr.Data["answer"] != float64(42) || res.Metadata()["model"] != "claude-test" {
		t.Fatalf("result = %#v", res)
	}
}

// A base URL works with or without the /v1 suffix.
func TestBaseURLWithV1Suffix(t *testing.T) {
	var s seen
	srv := jsonServer(t, 200, message("ok", "end_turn", 1, 1, ""), &s)
	if _, err := NewChatGenerator("k", "m").WithBaseURL(srv.URL+"/v1").Generate(context.Background(), loom.GenerateRequest{UserPrompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if s.path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", s.path)
	}
}

func TestPerRequestOverrides(t *testing.T) {
	var s seen
	srv := jsonServer(t, 200, message("ok", "end_turn", 1, 1, ""), &s)
	// Default base URL is unreachable; the request must still land via the
	// base_url override, with the per-request key and model winning over defaults.
	g := NewChatGenerator("default-key", "default-model").WithBaseURL("http://invalid.invalid").WithAllowedBaseURLs(srv.URL)
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{
		UserPrompt: "hi",
		Overrides:  map[string]any{"api_key": "override-key", "model": "override-model", "base_url": srv.URL},
	}); err != nil {
		t.Fatal(err)
	}
	if s.bearer != "override-key" || s.xAPIKey != "override-key" || s.body["model"] != "override-model" {
		t.Fatalf("overrides not applied: %+v", s)
	}
	// The override copy must NOT mutate the generator: a no-override call uses defaults.
	g.WithBaseURL(srv.URL)
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if s.bearer != "default-key" || s.body["model"] != "default-model" {
		t.Fatalf("override leaked into the generator: %+v", s)
	}
}

func TestGenerateStreamWithOverrides(t *testing.T) {
	var s seen
	srv := sseServer(t, sseStream(1, 1, "end_turn", "", "ok"), &s)
	// Streaming is the primary chat path: the per-request overrides must reach the
	// wire there too (not just in the sync Generate).
	g := NewChatGenerator("default-key", "default-model").WithBaseURL("http://invalid.invalid").WithAllowedBaseURLs(srv.URL)
	chunks, results, err := g.GenerateStream(context.Background(), loom.GenerateRequest{
		UserPrompt: "hi",
		Overrides:  map[string]any{"api_key": "override-key", "model": "override-model", "base_url": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(chunks, results)
	if s.bearer != "override-key" || s.body["model"] != "override-model" {
		t.Fatalf("streaming overrides not applied: %+v", s)
	}
	g.WithBaseURL(srv.URL)
	chunks, results, err = g.GenerateStream(context.Background(), loom.GenerateRequest{UserPrompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	drain(chunks, results)
	if s.bearer != "default-key" || s.body["model"] != "default-model" {
		t.Fatalf("override leaked into the streaming generator: %+v", s)
	}
}

// A base_url override that is neither the configured URL nor allowlisted must be
// rejected, and no request (carrying the real API key) may be sent to it. This
// guards against SSRF / API-key exfiltration via a caller-controlled base_url.
func TestBaseURLOverrideRejectedUnlessAllowlisted(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer srv.Close()
	g := NewChatGenerator("secret-key", "m") // no WithAllowedBaseURLs
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: "hi", Overrides: map[string]any{"base_url": srv.URL}}); err == nil {
		t.Fatal("expected base_url override to be rejected, got nil error")
	}
	if reached {
		t.Fatal("request reached the un-allowlisted host — API key would have leaked")
	}
}

// ANTHROPIC_* environment variables must not redirect requests or swap keys.
func TestEnvironmentIsIgnored(t *testing.T) {
	var s seen
	srv := jsonServer(t, 200, message("ok", "end_turn", 1, 1, ""), &s)
	t.Setenv("ANTHROPIC_BASE_URL", "http://invalid.invalid")
	t.Setenv("ANTHROPIC_API_KEY", "env-key")
	if _, err := NewChatGenerator("configured", "m").WithBaseURL(srv.URL).Generate(context.Background(), loom.GenerateRequest{UserPrompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if s.bearer != "configured" {
		t.Fatalf("key = %q, want the configured key", s.bearer)
	}
}

func TestRefusalIsRejected(t *testing.T) {
	srv := jsonServer(t, 200, message("", "refusal", 1, 0, `,"stop_details":{"type":"refusal","category":"cyber","explanation":"declined"}`), nil)
	_, err := NewChatGenerator("k", "m").WithBaseURL(srv.URL).Generate(context.Background(), loom.GenerateRequest{UserPrompt: "q"})
	var ge *loom.GenerationError
	if !errors.As(err, &ge) || ge.Kind != loom.GenerationRejected {
		t.Fatalf("err = %v, want GenerationRejected", err)
	}
	if !strings.Contains(ge.Message, "cyber") || !strings.Contains(ge.Message, "declined") {
		t.Fatalf("message %q lacks the stop details", ge.Message)
	}
}

func TestStreamingRefusalFailsResult(t *testing.T) {
	srv := sseServer(t, sseStream(3, 2, "refusal", `,"stop_details":{"type":"refusal","category":"bio","explanation":null}`, "partial"), nil)
	chunks, results, err := NewChatGenerator("k", "m").WithBaseURL(srv.URL).GenerateStream(context.Background(), loom.GenerateRequest{UserPrompt: "q"})
	if err != nil {
		t.Fatal(err)
	}
	_, res := drain(chunks, results)
	if res.Status() != loom.ResultStatusFailed {
		t.Fatalf("status = %q, want failed", res.Status())
	}
	if msg, _ := res.Metadata()["error"].(string); !strings.Contains(msg, "bio") {
		t.Fatalf("error metadata %q lacks the category", msg)
	}
}

func TestEffortAndDefaults(t *testing.T) {
	var s seen
	srv := jsonServer(t, 200, message("ok", "end_turn", 1, 1, ""), &s)
	g := NewChatGenerator("k", "m").WithBaseURL(srv.URL)
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: "q", Params: loom.GenerateParams{Extra: map[string]any{"effort": "high"}}}); err != nil {
		t.Fatal(err)
	}
	oc, _ := s.body["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Fatalf("output_config = %v, want effort high", s.body["output_config"])
	}
	if s.body["max_tokens"] != float64(defaultMaxTokens) {
		t.Fatalf("max_tokens = %v, want %d", s.body["max_tokens"], defaultMaxTokens)
	}
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: "q", Params: loom.GenerateParams{Extra: map[string]any{"effort": "extreme"}}}); err == nil {
		t.Fatal("invalid effort accepted")
	}
}

func TestNativeStructuredOutputs(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}, "required": []any{"answer"}, "additionalProperties": false}
	var s seen
	srv := jsonServer(t, 200, message(`{"answer":42}`, "end_turn", 1, 1, ""), &s)
	res, err := NewChatGenerator("k", "m").WithBaseURL(srv.URL).WithNativeStructuredOutputs().Generate(context.Background(), loom.GenerateRequest{
		SystemPrompt: "be helpful", UserPrompt: "q", ResponseFormat: &loom.ResponseFormat{Schema: schema},
	})
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := s.body["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"] == nil {
		t.Fatalf("output_config = %v, want a json_schema format", s.body["output_config"])
	}
	if systemText(s.body) != "be helpful" {
		t.Fatalf("system = %q; native mode must not inject the prompt-steering text", systemText(s.body))
	}
	if sr, ok := res.(*loom.StructuredResult); !ok || sr.Data["answer"] != float64(42) {
		t.Fatalf("result = %#v", res)
	}
}

func TestVerify(t *testing.T) {
	var s seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r, &s)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case s.bearer != "good":
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		case r.URL.Path != "/v1/models/claude-opus-5-5":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`)
		default:
			io.WriteString(w, `{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5","created_at":"2026-10-01T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000}`)
		}
	}))
	defer srv.Close()
	g := NewChatGenerator("good", "claude-opus-5-5").WithBaseURL(srv.URL)
	info, err := g.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.ContextWindow != 1000000 || info.MaxOutputTokens != 128000 || info.DisplayName != "Claude Opus 5.5" {
		t.Fatalf("info %+v", info)
	}
	if _, err := g.Verify(context.Background(), map[string]any{"api_key": "user-key"}); !errors.Is(err, loom.ErrKeyRejected) {
		t.Fatalf("bad override key: err = %v, want ErrKeyRejected", err)
	}
	if s.bearer != "user-key" {
		t.Fatalf("override key not used: %q", s.bearer)
	}
	if _, err := g.Verify(context.Background(), map[string]any{"model": "nope"}); err == nil || errors.Is(err, loom.ErrKeyRejected) {
		t.Fatalf("unknown model: err = %v, want a not-found error", err)
	}
}
