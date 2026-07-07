package openai

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

func TestPerRequestOverrides(t *testing.T) {
	var gotKey, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		gotModel, _ = body["model"].(string)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
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
	var gotKey, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
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
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
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
