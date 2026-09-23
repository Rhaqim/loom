// Command media-story runs a Go-template web UI for a Loom-powered, multi-modal
// narrative. It uses local generators by default so every modality is runnable
// without credentials; setting MESHY_API_KEY adds a real 3D-generation agent.
package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/generator/meshy"
	"github.com/rhaqim/loom/schema"
	_ "modernc.org/sqlite"
)

//go:embed templates/index.html
var templateFiles embed.FS

const storyTitle = "The Boy Who Lived Beneath a Dying Sun"

type storyAgent struct {
	Slug     string
	Name     string
	Model    string
	Modality loom.Modality
}

type app struct {
	engine *loom.Engine
	agents map[string]storyAgent
	tmpl   *template.Template
}

type pageData struct {
	Title    string
	Agents   []storyAgent
	Selected string
	Prompt   string
	Output   *output
	Error    string
}

type output struct {
	Agent   storyAgent
	Text    string
	URL     string
	Preview string
	World   []loom.WorldDelta
	Pending bool
}

func main() {
	ctx := context.Background()
	db, err := sql.Open("sqlite", env("DB_PATH", "db.sqlite"))
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		log.Fatal(err)
	}

	gens := map[string]loom.Generator{
		"local-narrator": localGenerator{modality: loom.ModalityText},
		"local-image":    localGenerator{modality: loom.ModalityImage},
		"local-audio":    localGenerator{modality: loom.ModalityAudio},
		"local-video":    localGenerator{modality: loom.ModalityVideo},
		"local-model3d":  localGenerator{modality: loom.ModalityModel3D},
		"local-world":    localGenerator{modality: loom.ModalityWorld},
	}
	if key := os.Getenv("MESHY_API_KEY"); key != "" {
		gens["meshy-3d"] = meshy.NewModel3DGenerator(key)
	}
	e, err := loom.New(loom.Config{
		DB: db, Dialect: loom.DialectSQLite, Generators: gens,
		AsyncPoller: loom.PollerConfig{Interval: 3 * time.Second, Workers: 1},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer e.Close(ctx)

	a := &app{engine: e, agents: defaultAgents(os.Getenv("MESHY_API_KEY") != "")}
	if err := a.seed(ctx); err != nil {
		log.Fatal(err)
	}
	tmpl, err := template.ParseFS(templateFiles, "templates/index.html")
	if err != nil {
		log.Fatal(err)
	}
	a.tmpl = tmpl

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.index)
	mux.HandleFunc("POST /generate", a.generate)
	addr := ":" + env("PORT", "8080")
	log.Printf("media-story listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func defaultAgents(hasMeshy bool) map[string]storyAgent {
	agents := map[string]storyAgent{
		"narrator":        {"narrator", "Narrator", "local-narrator", loom.ModalityText},
		"concept-artist":  {"concept-artist", "Concept artist", "local-image", loom.ModalityImage},
		"sound-mage":      {"sound-mage", "Sound mage", "local-audio", loom.ModalityAudio},
		"cinematographer": {"cinematographer", "Cinematographer", "local-video", loom.ModalityVideo},
		"asset-forger":    {"asset-forger", "Asset forger", "local-model3d", loom.ModalityModel3D},
		"world-smith":     {"world-smith", "World smith", "local-world", loom.ModalityWorld},
	}
	if hasMeshy {
		agents["meshy-forger"] = storyAgent{"meshy-forger", "Meshy asset forger", "meshy-3d", loom.ModalityModel3D}
	}
	return agents
}

func (a *app) seed(ctx context.Context) error {
	for _, agent := range a.agents {
		sys := &loom.Prompt{Slug: agent.Slug + "-system", Version: 1, Kind: loom.PromptKindSystem,
			Body: "You contribute " + string(agent.Modality) + " to a dark-fantasy science-fiction story."}
		user := &loom.Prompt{Slug: agent.Slug + "-user", Version: 1, Kind: loom.PromptKindUserTemplate,
			Body: "Story: " + storyTitle + "\nScene request: {{.Action.Payload.prompt}}"}
		if err := a.engine.Prompts().Create(ctx, sys); err != nil {
			return err
		}
		if err := a.engine.Prompts().Create(ctx, user); err != nil {
			return err
		}
		if err := a.engine.Agents().Create(ctx, &loom.Agent{Slug: agent.Slug, Version: 1, Modal: agent.Modality,
			GeneratorSlug: agent.Model, SystemPromptID: sys.ID, UserTemplateID: user.ID}); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	a.render(w, pageData{Title: storyTitle, Agents: a.sortedAgents(), Selected: "narrator", Prompt: "Harry raises his wand as the thunder of an Imperial procession rolls through the ruined basilica."})
}

func (a *app) generate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.render(w, pageData{Title: storyTitle, Agents: a.sortedAgents(), Error: "Invalid form."})
		return
	}
	selected, prompt := r.FormValue("agent"), strings.TrimSpace(r.FormValue("prompt"))
	agent, ok := a.agents[selected]
	if !ok || prompt == "" || len(prompt) > 4000 {
		a.render(w, pageData{Title: storyTitle, Agents: a.sortedAgents(), Selected: selected, Prompt: prompt, Error: "Choose an agent and enter a scene prompt (up to 4,000 characters)."})
		return
	}

	ctx := r.Context()
	sess := &loom.Session{PlatformID: "media-story-web", State: loom.State{Modality: loom.ModalityText}}
	if err := a.engine.Sessions().Create(ctx, sess); err != nil {
		a.render(w, pageData{Title: storyTitle, Agents: a.sortedAgents(), Selected: selected, Prompt: prompt, Error: err.Error()})
		return
	}
	step, err := a.engine.RunStep(ctx, sess, loom.StepRequest{AgentSlug: agent.Slug, Action: &loom.Action{
		Kind: loom.ActionFreeText, Payload: map[string]any{"prompt": prompt},
	}})
	if err != nil {
		a.render(w, pageData{Title: storyTitle, Agents: a.sortedAgents(), Selected: selected, Prompt: prompt, Error: err.Error()})
		return
	}
	a.render(w, pageData{Title: storyTitle, Agents: a.sortedAgents(), Selected: selected, Prompt: prompt, Output: present(agent, step.Result)})
}

func (a *app) sortedAgents() []storyAgent {
	out := make([]storyAgent, 0, len(a.agents))
	for _, agent := range a.agents {
		out = append(out, agent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *app) render(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.Execute(w, data); err != nil {
		log.Printf("render: %v", err)
	}
}

func present(agent storyAgent, result loom.Result) *output {
	out := &output{Agent: agent, Pending: result.Status() == loom.ResultStatusPending}
	switch r := result.(type) {
	case *loom.TextResult:
		out.Text = r.Content
	case *loom.ImageResult:
		out.URL = r.URL
	case *loom.AudioResult:
		out.URL = r.URL
	case *loom.VideoResult:
		out.URL, out.Preview = r.URL, r.PreviewImage
	case *loom.Model3DResult:
		out.URL, out.Preview = r.URL, r.PreviewImage
	case *loom.WorldResult:
		out.World = r.Deltas
	}
	return out
}

type localGenerator struct{ modality loom.Modality }

func (g localGenerator) Modality() loom.Modality { return g.modality }

func (g localGenerator) Generate(_ context.Context, req loom.GenerateRequest) (loom.Result, error) {
	prompt := req.UserPrompt
	switch g.modality {
	case loom.ModalityText:
		return loom.NewTextResult("Beneath the corpse-sun, Harry's wandlight cut a clean silver line through the incense-dark. The approaching Space Marines did not kneel; they merely watched the boy who had survived a darkness they understood too well.", "stop", 0, 0), nil
	case loom.ModalityImage:
		return loom.NewImageResult(svg("Concept art: a wand-bearing wizard in a ruined gothic basilica"), prompt, 1024, 576), nil
	case loom.ModalityAudio:
		return loom.NewAudioResult("https://example.invalid/media-story/imperial-choir.wav", "audio/wav", 18), nil
	case loom.ModalityVideo:
		return loom.NewVideoResult("https://example.invalid/media-story/basilica.mp4", svg("Video preview: rain and ash fall over the basilica"), 8, 1280, 720), nil
	case loom.ModalityModel3D:
		return loom.NewModel3DResult("https://threejs.org/examples/models/gltf/DamagedHelmet/glTF-Binary/DamagedHelmet.glb", "model/gltf-binary", svg("3D asset: a rune-etched vox reliquary"), prompt, 2400, false), nil
	case loom.ModalityWorld:
		return loom.NewWorldResult([]loom.WorldDelta{
			{Op: "add_entity", Type: "wizard", EntityID: "harry", Pos: [3]float64{0, 0, 0}},
			{Op: "add_entity", Type: "space_marine", EntityID: "aurelian-guard", Pos: [3]float64{5, 0, -3}},
			{Op: "set_lighting", Data: map[string]any{"mood": "ashen dusk", "intensity": 0.3}},
		}), nil
	default:
		return nil, fmt.Errorf("unsupported demo modality %q", g.modality)
	}
}

func svg(label string) string {
	return "data:image/svg+xml," + url.QueryEscape(`<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="576"><rect width="100%" height="100%" fill="#17131f"/><text x="60" y="280" fill="#e6c985" font-family="serif" font-size="32">`+label+`</text></svg>`)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
