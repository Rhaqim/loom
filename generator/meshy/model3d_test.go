package meshy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	loom "github.com/rhaqim/loom"
)

func TestModel3DGeneratorTextFlow(t *testing.T) {
	var requests []struct {
		Path string
		Body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			requests = append(requests, struct {
				Path string
				Body map[string]any
			}{r.URL.Path, body})
			if body["mode"] == "refine" {
				_, _ = w.Write([]byte(`{"result":"refine-id"}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":"preview-id"}`))
			return
		}
		switch r.URL.Path {
		case "/openapi/v2/text-to-3d/preview-id":
			_, _ = w.Write([]byte(`{"status":"SUCCEEDED"}`))
		case "/openapi/v2/text-to-3d/refine-id":
			_, _ = w.Write([]byte(`{"status":"SUCCEEDED","prompt":"a chapel","thumbnail_url":"https://cdn/chapel.png","model_urls":{"glb":"https://cdn/chapel.glb"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	g := NewModel3DGenerator("key").WithBaseURL(srv.URL)
	res, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: "a chapel"})
	if err != nil {
		t.Fatal(err)
	}
	res, err = g.Poll(context.Background(), *res.TaskHandle())
	if err != nil {
		t.Fatal(err)
	}
	res, err = g.Poll(context.Background(), *res.TaskHandle())
	if err != nil {
		t.Fatal(err)
	}
	model, ok := res.(*loom.Model3DResult)
	if !ok || model.URL != "https://cdn/chapel.glb" || model.Prompt != "a chapel" {
		t.Fatalf("result = %#v", res)
	}
	if len(requests) != 2 || requests[0].Body["mode"] != "preview" || requests[1].Body["preview_task_id"] != "preview-id" {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestModel3DGeneratorImageFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/openapi/v1/image-to-3d" {
				t.Errorf("POST path = %s", r.URL.Path)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["image_url"] != "https://cdn/source.png" {
				t.Errorf("image_url = %#v", body["image_url"])
			}
			_, _ = w.Write([]byte(`{"result":"image-id"}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"status":"SUCCEEDED","thumbnail_url":"https://cdn/model.png","model_urls":{"glb":"https://cdn/model.glb"}}`))
		}
	}))
	defer srv.Close()

	g := NewModel3DGenerator("key").WithBaseURL(srv.URL)
	res, err := g.Generate(context.Background(), loom.GenerateRequest{Params: loom.GenerateParams{Extra: map[string]any{"image_url": "https://cdn/source.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err = g.Poll(context.Background(), *res.TaskHandle())
	if err != nil {
		t.Fatal(err)
	}
	if model, ok := res.(*loom.Model3DResult); !ok || model.URL != "https://cdn/model.glb" {
		t.Fatalf("result = %#v", res)
	}
}

func TestModel3DGeneratorRefineOptions(t *testing.T) {
	var preview, refine map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["mode"] == "refine" {
				refine = body
				_, _ = w.Write([]byte(`{"result":"refine-id"}`))
				return
			}
			preview = body
			_, _ = w.Write([]byte(`{"result":"preview-id"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"SUCCEEDED"}`))
	}))
	defer srv.Close()

	g := NewModel3DGenerator("key").WithBaseURL(srv.URL)
	res, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: "a chapel", Params: loom.GenerateParams{Extra: map[string]any{
		"art_style": "realistic", "refine_enable_pbr": true, "refine_texture_prompt": "mossy stone",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if preview["art_style"] != "realistic" || preview["enable_pbr"] != nil || preview["refine_enable_pbr"] != nil {
		t.Fatalf("preview body = %#v; refine_ options must not reach the preview", preview)
	}
	if _, err := g.Poll(context.Background(), *res.TaskHandle()); err != nil {
		t.Fatal(err)
	}
	if refine["enable_pbr"] != true || refine["texture_prompt"] != "mossy stone" || refine["preview_task_id"] != "preview-id" {
		t.Fatalf("refine body = %#v", refine)
	}
}

func TestModel3DGeneratorRejectsLongPrompt(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	g := NewModel3DGenerator("key").WithBaseURL(srv.URL)
	long := make([]byte, MaxPromptChars+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := g.Generate(context.Background(), loom.GenerateRequest{UserPrompt: string(long)}); err == nil {
		t.Fatal("over-limit prompt accepted")
	}
	if called {
		t.Fatal("over-limit prompt was submitted to Meshy")
	}
}
