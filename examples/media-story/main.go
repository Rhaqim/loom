// Command media-story runs a Go-template web UI for a Loom-powered, multi-modal
// narrative. It uses local generators by default so every modality is runnable
// without credentials; setting MESHY_API_KEY adds a real 3D-generation agent.
package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/generator/meshy"
	"github.com/rhaqim/loom/schema"
	_ "modernc.org/sqlite"
)

//go:embed templates/index.html
var templateFiles embed.FS

const storyTitle = "The Lamplighter Beneath a Dying Sun"

// defaultPrompt opens the story's first scene.
const defaultPrompt = "Wren lifts her lantern as the Ashen Choir's procession thunders through the drowned cathedral-ship."

// maxPromptChars bounds free-text input for the local agents.
const maxPromptChars = 4000

// meshyJobUSD is an assumed per-job price for the Meshy agent (preview plus
// refine), used so the demo's cost summary can show real spend. Replace it with
// your plan's actual credit cost.
const meshyJobUSD = 0.40

type storyAgent struct {
	Slug     string
	Name     string
	Model    string
	Modality loom.Modality
	// MaxPrompt caps the scene prompt for this agent. The Meshy agent uses
	// Meshy's own limit because its user template passes the prompt through
	// unwrapped.
	MaxPrompt int
	// RawPrompt sends the scene prompt as-is instead of wrapping it in the
	// story context, for providers with tight prompt limits.
	RawPrompt bool
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
	// Refresh, when set, reloads the page at this URL so a pending async job's
	// result appears without the user resubmitting.
	Refresh string
}

type output struct {
	Agent   storyAgent
	Text    string
	URL     template.URL
	Preview template.URL
	World   []loom.WorldDelta
	Pending bool
	TaskID  string
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
	hasMeshy := os.Getenv("MESHY_API_KEY") != ""
	if hasMeshy {
		gens["meshy-3d"] = meshy.NewModel3DGenerator(os.Getenv("MESHY_API_KEY"))
	}
	a, err := newApp(ctx, db, gens, defaultAgents(hasMeshy), loom.PollerConfig{
		// Meshy's preview and refine stages each take minutes. Poll every 10s
		// and allow 90 polls per stage (Loom resets the count when a job moves
		// to its next stage), i.e. up to 15 minutes each.
		Interval: 10 * time.Second, Workers: 1, MaxAttempts: 90,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer a.engine.Close(ctx)

	addr := ":" + env("PORT", "8080")
	log.Printf("media-story listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, a.routes()))
}

// newApp builds the engine, seeds the agents and parses the template.
func newApp(ctx context.Context, db *sql.DB, gens map[string]loom.Generator, agents map[string]storyAgent, poller loom.PollerConfig) (*app, error) {
	e, err := loom.New(loom.Config{
		DB: db, Dialect: loom.DialectSQLite, Generators: gens,
		AsyncPoller: poller,
		JobPricing:  map[string]float64{"meshy-3d": meshyJobUSD},
	})
	if err != nil {
		return nil, err
	}
	a := &app{engine: e, agents: agents}
	if err := a.seed(ctx); err != nil {
		return nil, err
	}
	// The poller does not run until started; without this, async jobs (Meshy)
	// stay pending forever. Engine.Close stops it and waits for in-flight polls.
	e.StartPoller(context.WithoutCancel(ctx))
	if a.tmpl, err = template.ParseFS(templateFiles, "templates/index.html"); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("POST /generate", a.generate)
	mux.HandleFunc("GET /task/{id}", a.task)
	return mux
}

func defaultAgents(hasMeshy bool) map[string]storyAgent {
	agents := map[string]storyAgent{
		"narrator":        {Slug: "narrator", Name: "Narrator", Model: "local-narrator", Modality: loom.ModalityText},
		"concept-artist":  {Slug: "concept-artist", Name: "Concept artist", Model: "local-image", Modality: loom.ModalityImage},
		"sound-mage":      {Slug: "sound-mage", Name: "Sound mage", Model: "local-audio", Modality: loom.ModalityAudio},
		"cinematographer": {Slug: "cinematographer", Name: "Cinematographer", Model: "local-video", Modality: loom.ModalityVideo},
		"asset-forger":    {Slug: "asset-forger", Name: "Asset forger", Model: "local-model3d", Modality: loom.ModalityModel3D},
		"world-smith":     {Slug: "world-smith", Name: "World smith", Model: "local-world", Modality: loom.ModalityWorld},
	}
	if hasMeshy {
		agents["meshy-forger"] = storyAgent{Slug: "meshy-forger", Name: "Meshy asset forger", Model: "meshy-3d",
			Modality: loom.ModalityModel3D, MaxPrompt: meshy.MaxPromptChars, RawPrompt: true}
	}
	for slug, a := range agents {
		if a.MaxPrompt == 0 {
			a.MaxPrompt = maxPromptChars
			agents[slug] = a
		}
	}
	return agents
}

// seed creates each agent and its prompts unless they already exist, so the
// server restarts cleanly against its persisted database.
func (a *app) seed(ctx context.Context) error {
	for _, agent := range a.agents {
		userBody := "Story: " + storyTitle + "\nScene request: {{.Action.Payload.prompt}}"
		if agent.RawPrompt {
			userBody = "{{.Action.Payload.prompt}}"
		}
		sys, err := a.ensurePrompt(ctx, &loom.Prompt{Slug: agent.Slug + "-system", Version: 1, Kind: loom.PromptKindSystem,
			Body: "You contribute " + string(agent.Modality) + " to a dark-fantasy science-fiction story."})
		if err != nil {
			return err
		}
		user, err := a.ensurePrompt(ctx, &loom.Prompt{Slug: agent.Slug + "-user", Version: 1, Kind: loom.PromptKindUserTemplate, Body: userBody})
		if err != nil {
			return err
		}
		if _, err := a.engine.Agents().Get(ctx, "", agent.Slug, 1); err == nil {
			continue
		} else if !errors.Is(err, loom.ErrNotFound) {
			return err
		}
		if err := a.engine.Agents().Create(ctx, &loom.Agent{Slug: agent.Slug, Version: 1, Modal: agent.Modality,
			GeneratorSlug: agent.Model, SystemPromptID: sys.ID, UserTemplateID: user.ID}); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) ensurePrompt(ctx context.Context, p *loom.Prompt) (*loom.Prompt, error) {
	existing, err := a.engine.Prompts().Get(ctx, "", p.Slug, p.Version)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, loom.ErrNotFound) {
		return nil, err
	}
	return p, a.engine.Prompts().Create(ctx, p)
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	a.render(w, pageData{Title: storyTitle, Agents: a.sortedAgents(), Selected: "narrator", Prompt: defaultPrompt})
}

func (a *app) generate(w http.ResponseWriter, r *http.Request) {
	page := pageData{Title: storyTitle, Agents: a.sortedAgents()}
	if err := r.ParseForm(); err != nil {
		page.Error = "Invalid form."
		a.render(w, page)
		return
	}
	page.Selected, page.Prompt = r.FormValue("agent"), strings.TrimSpace(r.FormValue("prompt"))
	agent, ok := a.agents[page.Selected]
	if !ok || page.Prompt == "" {
		page.Error = "Choose an agent and enter a scene prompt."
		a.render(w, page)
		return
	}
	if n := utf8.RuneCountInString(page.Prompt); n > agent.MaxPrompt {
		page.Error = fmt.Sprintf("%s accepts prompts up to %d characters; this one is %d.", agent.Name, agent.MaxPrompt, n)
		a.render(w, page)
		return
	}

	ctx := r.Context()
	sess := &loom.Session{PlatformID: "media-story-web", State: loom.State{Modality: loom.ModalityText}}
	if err := a.engine.Sessions().Create(ctx, sess); err != nil {
		page.Error = err.Error()
		a.render(w, page)
		return
	}
	step, err := a.engine.RunStep(ctx, sess, loom.StepRequest{AgentSlug: agent.Slug, Action: &loom.Action{
		Kind: loom.ActionFreeText, Payload: map[string]any{"prompt": page.Prompt},
	}})
	if err != nil {
		page.Error = err.Error()
		a.render(w, page)
		return
	}
	page.Output = present(agent, step.Result)
	if page.Output.Pending {
		page.Refresh = taskURL(page.Output.TaskID, agent.Slug)
	}
	a.render(w, page)
}

// task shows an async job's current state, reloading itself while it is
// pending so the generated asset appears once Loom's poller resolves it.
func (a *app) task(w http.ResponseWriter, r *http.Request) {
	page := pageData{Title: storyTitle, Agents: a.sortedAgents(), Prompt: defaultPrompt}
	id, err := uuid.Parse(r.PathValue("id"))
	agent, ok := a.agents[r.URL.Query().Get("agent")]
	if err != nil || !ok {
		w.WriteHeader(http.StatusNotFound)
		page.Error = "Unknown task."
		a.render(w, page)
		return
	}
	page.Selected = agent.Slug
	status, result, err := a.engine.TaskStatus(r.Context(), id)
	switch {
	case errors.Is(err, loom.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		page.Error = "Unknown task."
	case err != nil:
		page.Error = err.Error()
	case status == "failed":
		page.Error = agent.Name + ": generation failed."
	case status == "ready" && result != nil:
		page.Output = present(agent, result)
	default:
		page.Output = &output{Agent: agent, Pending: true, TaskID: id.String()}
		page.Refresh = taskURL(id.String(), agent.Slug)
	}
	a.render(w, page)
}

func taskURL(taskID, agentSlug string) string {
	return "/task/" + url.PathEscape(taskID) + "?agent=" + url.QueryEscape(agentSlug)
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
	if th := result.TaskHandle(); th != nil {
		out.TaskID = th.ID.String()
	}
	switch r := result.(type) {
	case *loom.TextResult:
		out.Text = r.Content
	case *loom.ImageResult:
		out.URL = mediaURL(r.URL)
		out.Preview = out.URL
	case *loom.AudioResult:
		out.URL = mediaURL(r.URL)
	case *loom.VideoResult:
		out.URL, out.Preview = mediaURL(r.URL), mediaURL(r.PreviewImage)
	case *loom.Model3DResult:
		out.URL, out.Preview = mediaURL(r.URL), mediaURL(r.PreviewImage)
	case *loom.WorldResult:
		out.World = r.Deltas
	}
	return out
}

// mediaURL marks a generated asset URL as safe for src/href attributes.
// html/template rewrites any data: URL to "#ZgotmplZ", which blanked the local
// SVG previews, so data:image/ URLs (inert inside <img>) and http(s) URLs are
// passed through. Anything else is dropped.
func mediaURL(s string) template.URL {
	switch {
	case strings.HasPrefix(s, "https://"), strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "data:image/"):
		return template.URL(s)
	}
	return ""
}

type localGenerator struct{ modality loom.Modality }

func (g localGenerator) Modality() loom.Modality { return g.modality }

func (g localGenerator) Generate(_ context.Context, req loom.GenerateRequest) (loom.Result, error) {
	prompt := req.UserPrompt
	switch g.modality {
	case loom.ModalityText:
		return loom.NewTextResult("Beneath the corpse-sun, Wren's lantern cut a clean silver line through the incense-dark. The armoured choristers did not kneel; they only watched the girl who had walked out of a darkness they understood too well.", "stop", 0, 0), nil
	case loom.ModalityImage:
		return loom.NewImageResult(svg("Concept art: a lantern-bearer in a drowned cathedral-ship"), prompt, 1024, 576), nil
	case loom.ModalityAudio:
		return loom.NewAudioResult("https://example.invalid/media-story/ashen-choir.wav", "audio/wav", 18), nil
	case loom.ModalityVideo:
		return loom.NewVideoResult("https://example.invalid/media-story/cathedral-ship.mp4", svg("Video preview: rain and ash fall through the nave"), 8, 1280, 720), nil
	case loom.ModalityModel3D:
		return loom.NewModel3DResult("https://threejs.org/examples/models/gltf/DamagedHelmet/glTF-Binary/DamagedHelmet.glb", "model/gltf-binary", svg("3D asset: a rune-etched choir helm"), prompt, 2400, false), nil
	case loom.ModalityWorld:
		return loom.NewWorldResult([]loom.WorldDelta{
			{Op: "add_entity", Type: "lamplighter", EntityID: "wren", Pos: [3]float64{0, 0, 0}},
			{Op: "add_entity", Type: "choir_knight", EntityID: "ashen-warden", Pos: [3]float64{5, 0, -3}},
			{Op: "set_lighting", Data: map[string]any{"mood": "ashen dusk", "intensity": 0.3}},
		}), nil
	default:
		return nil, fmt.Errorf("unsupported demo modality %q", g.modality)
	}
}

func svg(label string) string {
	return "data:image/svg+xml," + url.PathEscape(`<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="576"><rect width="100%" height="100%" fill="#17131f"/><text x="60" y="280" fill="#e6c985" font-family="serif" font-size="32">`+label+`</text></svg>`)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
