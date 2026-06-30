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
