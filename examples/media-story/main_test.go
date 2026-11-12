package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/schema"
)

// fakeMeshy is a two-stage async 3D generator whose adapter Provider ("meshy")
// differs from the slug it is registered under ("meshy-3d"), like the real one.
type fakeMeshy struct{ refineStarts *atomic.Int32 }

func (fakeMeshy) Modality() loom.Modality { return loom.ModalityModel3D }
func (fakeMeshy) Generate(context.Context, loom.GenerateRequest) (loom.Result, error) {
	return loom.NewPendingResult(loom.ModalityModel3D, &loom.TaskHandle{ID: uuid.New(), Provider: "meshy", Handle: "text:p"}), nil
}
func (f fakeMeshy) Poll(_ context.Context, h loom.TaskHandle) (loom.Result, error) {
	if h.Handle == "text:p" {
		f.refineStarts.Add(1)
		h.Handle = "refine:r"
		return loom.NewPendingResult(loom.ModalityModel3D, &h), nil
	}
	return loom.NewModel3DResult("https://cdn.example/helm.glb", "model/gltf-binary", "https://cdn.example/helm.png", "helm", 0, false), nil
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:mediastory_"+uuid.NewString()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := schema.NewLoader(schema.DialectSQLite).Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func testApp(t *testing.T, db *sql.DB, starts *atomic.Int32) *app {
	t.Helper()
	gens := map[string]loom.Generator{
		"local-narrator": localGenerator{modality: loom.ModalityText},
		"local-image":    localGenerator{modality: loom.ModalityImage},
		"local-audio":    localGenerator{modality: loom.ModalityAudio},
		"local-video":    localGenerator{modality: loom.ModalityVideo},
		"local-model3d":  localGenerator{modality: loom.ModalityModel3D},
		"local-world":    localGenerator{modality: loom.ModalityWorld},
		"meshy-3d":       fakeMeshy{starts},
	}
	a, err := newApp(context.Background(), db, gens, defaultAgents(true),
		loom.PollerConfig{Interval: 20 * time.Millisecond, Workers: 1, MaxAttempts: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.engine.Close(context.Background()) })
	return a
}

func post(t *testing.T, h http.Handler, agent, prompt string) string {
	t.Helper()
	form := url.Values{"agent": {agent}, "prompt": {prompt}}
	req := httptest.NewRequest(http.MethodPost, "/generate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

func get(h http.Handler, path string) string {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Body.String()
}

func TestRestartReusesSeededAgents(t *testing.T) {
	db := testDB(t)
	var starts atomic.Int32
	first := testApp(t, db, &starts)
	first.engine.Close(context.Background())
	testApp(t, db, &starts) // fails on a UNIQUE violation if seeding is not idempotent
}

func TestLocalModalitiesRender(t *testing.T) {
	var starts atomic.Int32
	h := testApp(t, testDB(t), &starts).routes()
	for slug := range defaultAgents(false) {
		body := post(t, h, slug, "a short scene")
		if strings.Contains(body, "ZgotmplZ") {
			t.Errorf("%s: html/template blanked a media URL", slug)
		}
		if strings.Contains(body, `class="error"`) {
			t.Errorf("%s: rendered an error", slug)
		}
	}
	if body := post(t, h, "asset-forger", "a helm"); !strings.Contains(body, "<model-viewer") {
		t.Error("3D result did not render a model-viewer")
	}
}

func TestMeshyPromptLimit(t *testing.T) {
	var starts atomic.Int32
	h := testApp(t, testDB(t), &starts).routes()
	body := post(t, h, "meshy-forger", strings.Repeat("a", 601))
	if !strings.Contains(body, "accepts prompts up to 600 characters") {
		t.Fatal("over-limit Meshy prompt was not rejected")
	}
	if starts.Load() != 0 {
		t.Fatal("over-limit prompt reached the generator")
	}
}

func TestAsyncJobResolvesOnTaskPage(t *testing.T) {
	var starts atomic.Int32
	h := testApp(t, testDB(t), &starts).routes()
	body := post(t, h, "meshy-forger", "a rune-etched helm")
	m := regexp.MustCompile(`url=(/task/[^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("pending result did not set a refresh to the task page:\n%s", body)
	}
	taskPath := strings.ReplaceAll(m[1], "&amp;", "&")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		page := get(h, taskPath)
		if strings.Contains(page, `src="https://cdn.example/helm.glb"`) {
			if starts.Load() != 1 {
				t.Fatalf("refine started %d times, want 1", starts.Load())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task never resolved; last page:\n%s", get(h, taskPath))
}
