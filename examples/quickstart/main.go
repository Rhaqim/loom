// Quickstart: how to use loom to run a multi-agent turn — a lead that "writes"
// and a follower that "reviews" it — with NO API key and NO external database
// (in-memory SQLite + a tiny local generator). Run: go run ./examples/quickstart
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/schema"
	_ "modernc.org/sqlite"
)

// storyGen stands in for "your model". loom hands it the rendered system + user
// prompt and it returns a result. Swap this for generator/openai or
// generator/anthropic and nothing else in this file changes.
type storyGen struct{}

func (storyGen) Modality() loom.Modality { return loom.ModalityText }

func (storyGen) Generate(_ context.Context, req loom.GenerateRequest) (loom.Result, error) {
	if strings.Contains(req.SystemPrompt, "critic") {
		// The follower received the lead's prose via {{.Inputs.Lead}}.
		return loom.NewTextResult("Strong hook — trim one adjective and it sings. 8/10.", "stop", 0, 0), nil
	}
	return loom.NewTextResult("You shoulder the creaking door open; cold air and the smell of old paper spill into the hall.", "stop", 0, 0), nil
}

func main() {
	ctx := context.Background()
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	// 1. Engine: in-memory SQLite + your generator. Zero infra, no API key.
	db, err := sql.Open("sqlite", "file:quickstart?mode=memory&cache=shared")
	must(err)
	db.SetMaxOpenConns(1)
	must(schema.NewLoader(schema.DialectSQLite).Apply(ctx, db))

	e, err := loom.New(loom.Config{
		DB: db, Dialect: loom.DialectSQLite,
		Generators: map[string]loom.Generator{"model": storyGen{}},
	})
	must(err)

	// 2. Prompts. User templates render with the turn's Inputs ({{.Inputs.x}}).
	authorSys := prompt(ctx, e, "author-sys", loom.PromptKindSystem, "You are a narrator. Write one vivid sentence.")
	authorUser := prompt(ctx, e, "author-user", loom.PromptKindUserTemplate, "The player does: {{.Inputs.action}}")
	criticSys := prompt(ctx, e, "critic-sys", loom.PromptKindSystem, "You are a critic. Review the scene.")
	criticUser := prompt(ctx, e, "critic-user", loom.PromptKindUserTemplate, "Review this scene: {{.Inputs.Lead}}")

	// 3. Agents: an author (the lead) and a critic (a follower).
	must(e.Agents().Create(ctx, &loom.Agent{Slug: "author", Version: 1, Modal: loom.ModalityText,
		GeneratorSlug: "model", SystemPromptID: authorSys, UserTemplateID: authorUser}))
	must(e.Agents().Create(ctx, &loom.Agent{Slug: "critic", Version: 1, Modal: loom.ModalityText,
		GeneratorSlug: "model", SystemPromptID: criticSys, UserTemplateID: criticUser}))

	// 4. A session (one playthrough), then one multi-agent turn.
	sess := &loom.Session{PlatformID: "demo", State: loom.State{Modality: loom.ModalityText}}
	must(e.Sessions().Create(ctx, sess))

	turn, err := e.RunTurn(ctx, sess, loom.TurnRequest{
		Flow: loom.Flow{
			Slug:      "story",
			Lead:      loom.FlowAgent{AgentSlug: "author", OutputKey: "Lead"}, // exposed to followers as {{.Inputs.Lead}}
			Followers: []loom.FlowAgent{{AgentSlug: "critic"}},                // runs in parallel after the lead
		},
		Inputs: map[string]any{"action": "open the creaking door"},
	})
	must(err)

	// 5. Read each agent's output.
	fmt.Println("ACTION   :", "open the creaking door")
	fmt.Println("AUTHOR   :", loom.ResultText(turn.Lead.Result))
	fmt.Println("CRITIC   :", loom.ResultText(turn.Followers["critic"].Result))
}

func prompt(ctx context.Context, e *loom.Engine, slug string, kind loom.PromptKind, body string) uuid.UUID {
	p := &loom.Prompt{Slug: slug, Version: 1, Kind: kind, Body: body}
	if err := e.Prompts().Create(ctx, p); err != nil {
		panic(err)
	}
	return p.ID
}
