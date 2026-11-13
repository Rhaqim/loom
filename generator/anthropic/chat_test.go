package anthropic

// chat_test.go — hermetic coverage for the Anthropic adapter (H8). The streaming
// path used to run a json.Decoder over the SSE body, which broke on the first
// "event:" line and silently returned empty content with zero tokens. These
// tests run a canned Anthropic SSE stream and a canned sync response through an
// httptest server (no real API) and assert correct parsing.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	loom "github.com/rhaqim/loom"
)

func TestStreamParsesSSEAndUsage(t *testing.T) {
	const sse = "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello "}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer srv.Close()

	g := NewChatGenerator("k", "claude-test").WithBaseURL(srv.URL)
	chunks, results, err := g.GenerateStream(context.Background(), loom.GenerateRequest{UserPrompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for c := range chunks {
		got.WriteString(c.Content)
	}
	res := <-results

	if got.String() != "Hello world" {
		t.Errorf("streamed chunks = %q, want %q", got.String(), "Hello world")
	}
	tr, ok := res.(*loom.TextResult)
	if !ok {
		t.Fatalf("result type %T, want *loom.TextResult", res)
	}
	if tr.Content != "Hello world" {
		t.Errorf("content = %q, want %q", tr.Content, "Hello world")
	}
	if tr.InputTokens != 12 || tr.OutputTokens != 5 {
		t.Errorf("usage in=%d out=%d, want 12/5", tr.InputTokens, tr.OutputTokens)
	}
	if tr.FinishReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn", tr.FinishReason)
	}
	if res.Metadata()["model"] != "claude-test" {
		t.Errorf("model metadata = %v, want claude-test", res.Metadata()["model"])
	}
}

func TestGenerateForwardsParamsAndSchema(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"content":[{"type":"text","text":"{\"answer\":42}"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`)
	}))
	defer srv.Close()

	g := NewChatGenerator("k", "claude-test").WithBaseURL(srv.URL)
	rf := &loom.ResponseFormat{Schema: map[string]any{"type": "object"}, StrictMode: true}
	res, err := g.Generate(context.Background(), loom.GenerateRequest{
		SystemPrompt:   "be helpful",
		UserPrompt:     "q",
		ResponseFormat: rf,
		Params:         loom.GenerateParams{Temperature: 0.4, TopP: 0.9},
	})
	if err != nil {
		t.Fatal(err)
	}

	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	sys, _ := sent["system"].(string)
	if !strings.Contains(sys, "JSON") || !strings.Contains(sys, `"type":"object"`) {
		t.Errorf("system prompt missing schema injection: %q", sys)
	}
	if sent["temperature"] != 0.4 {
		t.Errorf("temperature not forwarded: %v", sent["temperature"])
	}
	if sent["top_p"] != 0.9 {
		t.Errorf("top_p not forwarded: %v", sent["top_p"])
	}

	sr, ok := res.(*loom.StructuredResult)
	if !ok {
		t.Fatalf("result type %T, want *loom.StructuredResult", res)
	}
	if sr.Data["answer"] != float64(42) {
		t.Errorf("structured data = %v, want answer=42", sr.Data)
	}
	if res.Metadata()["model"] != "claude-test" {
		t.Errorf("model metadata = %v, want claude-test", res.Metadata()["model"])
	}
}

func TestPerRequestOverrides(t *testing.T) {
	var gotKey, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		gotModel, _ = body["model"].(string)
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	// Default base URL is unreachable; the request must still land via the
	// base_url override, with the per-request key and model winning over defaults.
	g := NewChatGenerator("default-key", "default-model").WithBaseURL("http://invalid.invalid").WithAllowedBaseURLs(srv.URL)
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{
		UserPrompt: "hi",
		Overrides:  map[string]any{"api_key": "override-key", "model": "override-model", "base_url": srv.URL},
	}); err != nil {
		t.Fatal(err)
	}
	if gotKey != "override-key" || gotModel != "override-model" {
		t.Fatalf("overrides not applied: key=%q model=%q", gotKey, gotModel)
	}

	// The override copy must NOT mutate the generator: a no-override call uses defaults.
	g.WithBaseURL(srv.URL)
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if gotKey != "default-key" || gotModel != "default-model" {
		t.Fatalf("override leaked into the generator: key=%q model=%q", gotKey, gotModel)
	}
}

func TestGenerateStreamWithOverrides(t *testing.T) {
	const sse = "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	var gotKey, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer srv.Close()

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
	for range chunks {
	}
	<-results
	if gotKey != "override-key" || gotModel != "override-model" {
		t.Fatalf("streaming overrides not applied: key=%q model=%q", gotKey, gotModel)
	}

	// The override copy must not mutate the generator: a no-override stream uses defaults.
	g.WithBaseURL(srv.URL)
	chunks2, results2, err := g.GenerateStream(context.Background(), loom.GenerateRequest{UserPrompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	for range chunks2 {
	}
	<-results2
	if gotKey != "default-key" || gotModel != "default-model" {
		t.Fatalf("override leaked into the streaming generator: key=%q model=%q", gotKey, gotModel)
	}
}

// A base_url override that is neither the configured URL nor allowlisted must be
// rejected, and no request (carrying the real API key) may be sent to it. This
// guards against SSRF / API-key exfiltration via a caller-controlled base_url.
func TestBaseURLOverrideRejectedUnlessAllowlisted(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer srv.Close()

	g := NewChatGenerator("secret-key", "m") // no WithAllowedBaseURLs
	_, err := g.Generate(context.Background(), loom.GenerateRequest{
		UserPrompt: "hi",
		Overrides:  map[string]any{"base_url": srv.URL},
	})
	if err == nil {
		t.Fatal("expected base_url override to be rejected, got nil error")
	}
	if reached {
		t.Fatal("request reached the un-allowlisted host — API key would have leaked")
	}
}

func captureServer(t *testing.T, reply string, got *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, got)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const okReply = `{"content":[{"type":"text","text":"{\"answer\":42}"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func TestRefusalIsRejected(t *testing.T) {
	var sent map[string]any
	srv := captureServer(t, `{"content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"declined"},"usage":{"input_tokens":1,"output_tokens":0}}`, &sent)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"category\":\"bio\"}},\"usage\":{\"output_tokens\":2}}\n\n")
	}))
	defer srv.Close()
	chunks, results, err := NewChatGenerator("k", "m").WithBaseURL(srv.URL).GenerateStream(context.Background(), loom.GenerateRequest{UserPrompt: "q"})
	if err != nil {
		t.Fatal(err)
	}
	for range chunks {
	}
	res := <-results
	if res.Status() != loom.ResultStatusFailed {
		t.Fatalf("status = %q, want failed", res.Status())
	}
	if msg, _ := res.Metadata()["error"].(string); !strings.Contains(msg, "bio") {
		t.Fatalf("error metadata %q lacks the category", msg)
	}
}

func TestEffortAndDefaults(t *testing.T) {
	var sent map[string]any
	srv := captureServer(t, okReply, &sent)
	g := NewChatGenerator("k", "m").WithBaseURL(srv.URL)
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: "q", Params: loom.GenerateParams{Extra: map[string]any{"effort": "high"}}}); err != nil {
		t.Fatal(err)
	}
	oc, _ := sent["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Fatalf("output_config = %v, want effort high", sent["output_config"])
	}
	if sent["max_tokens"] != float64(defaultMaxTokens) {
		t.Fatalf("max_tokens = %v, want %d", sent["max_tokens"], defaultMaxTokens)
	}
	if _, ok := sent["temperature"]; ok {
		t.Fatal("unset temperature was sent; some models reject sampling params")
	}
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: "q", Params: loom.GenerateParams{Extra: map[string]any{"effort": "extreme"}}}); err == nil {
		t.Fatal("invalid effort accepted")
	}
}

func TestNativeStructuredOutputs(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}, "required": []any{"answer"}, "additionalProperties": false}
	var sent map[string]any
	srv := captureServer(t, okReply, &sent)
	res, err := NewChatGenerator("k", "m").WithBaseURL(srv.URL).WithNativeStructuredOutputs().Generate(context.Background(), loom.GenerateRequest{
		SystemPrompt: "be helpful", UserPrompt: "q", ResponseFormat: &loom.ResponseFormat{Schema: schema},
	})
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := sent["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"] == nil {
		t.Fatalf("output_config = %v, want a json_schema format", sent["output_config"])
	}
	if sent["system"] != "be helpful" {
		t.Fatalf("system = %q; native mode must not inject the prompt-steering text", sent["system"])
	}
	if sr, ok := res.(*loom.StructuredResult); !ok || sr.Data["answer"] != float64(42) {
		t.Fatalf("result = %#v", res)
	}
}

func TestVerify(t *testing.T) {
	var path, key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("x-api-key")
		switch key {
		case "good":
			io.WriteString(w, `{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5","max_input_tokens":1000000,"max_tokens":128000}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
		}
	}))
	defer srv.Close()
	g := NewChatGenerator("good", "claude-opus-5-5").WithBaseURL(srv.URL)
	info, err := g.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/models/claude-opus-5-5" || info.ContextWindow != 1000000 || info.MaxOutputTokens != 128000 || info.DisplayName != "Claude Opus 5.5" {
		t.Fatalf("path %q info %+v", path, info)
	}
	if _, err := g.Verify(context.Background(), map[string]any{"api_key": "user-key"}); !errors.Is(err, loom.ErrKeyRejected) {
		t.Fatalf("bad override key: err = %v, want ErrKeyRejected", err)
	}
	if key != "user-key" {
		t.Fatalf("override key not used: %q", key)
	}
}
