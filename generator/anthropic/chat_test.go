package anthropic

// chat_test.go — hermetic coverage for the Anthropic adapter (H8). The streaming
// path used to run a json.Decoder over the SSE body, which broke on the first
// "event:" line and silently returned empty content with zero tokens. These
// tests run a canned Anthropic SSE stream and a canned sync response through an
// httptest server (no real API) and assert correct parsing.

import (
	"context"
	"encoding/json"
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
	g := NewChatGenerator("default-key", "default-model").WithBaseURL("http://invalid.invalid")
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
