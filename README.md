# loom

A modality-agnostic engine for orchestrating multi-agent AI applications.

Loom handles the infrastructure — session management, versioned agents and prompts, hook pipelines, cost tracking, branch/replay, LLM-as-judge scoring, and a first-class test harness — so you can focus on your product, not the plumbing.

> **Using an AI assistant (Claude, Copilot, Cursor, …) to build with loom?** Point it at
> [`llms.txt`](llms.txt) — a self-contained, task-oriented guide to loom's API written for LLMs.

---

## Features

| | |
|---|---|
| **Modality-agnostic** | Text, image, video, and structured output in one pipeline |
| **Versioned agents & prompts** | Slug + version addressing; swap models without touching session history |
| **Session branching & replay** | Fork at any step; explore alternatives; GC stale branches automatically |
| **Hook bus** | Pre- and post-hooks for validation, retry logic, and content filtering |
| **Cost tracking & budgets** | Per-step token/USD recording; time-windowed budget enforcement |
| **Judge subsystem** | Rubric scoring, pairwise comparison, and binary constraints via LLM |
| **Test harness** | YAML-driven test plans, variant matrices, parallel execution, assertion DSL |
| **Multiple generators** | OpenAI, Anthropic, Replicate (images), Runway (video), plus an echo stub |
| **Postgres + SQLite** | Idempotent schema loader; bring your own `*sql.DB` |

---

## Installation

```bash
go get github.com/rhaqim/loom
```

Requires Go 1.24+ (the module targets Go 1.25; see `go.mod`) and a Postgres or SQLite database.

---

## Quick start

```go
package main

import (
    "context"
    "database/sql"
    "fmt"
    "log"
    "os"

    _ "github.com/lib/pq"

    loom "github.com/rhaqim/loom"
    "github.com/rhaqim/loom/generator/openai"
    "github.com/rhaqim/loom/schema"
)

func main() {
    db, _ := sql.Open("postgres", "postgres://user:pass@localhost/mydb?sslmode=disable")

    ctx := context.Background()

    // Apply the loom schema (idempotent).
    schema.NewLoader(schema.DialectPostgres).Apply(ctx, db)

    // Create the engine.
    e, _ := loom.New(loom.Config{
        DB:      db,
        Dialect: loom.DialectPostgres,
        Generators: map[string]loom.Generator{
            "gpt4o": openai.NewChatGenerator(os.Getenv("OPENAI_API_KEY"), "gpt-4o"),
        },
    })

    // Seed a prompt and agent (idempotent in practice via slug+version).
    e.Prompts().Create(ctx, &loom.Prompt{
        Slug: "assistant-sys", Version: 1, Kind: loom.PromptKindSystem,
        Body: "You are a vivid storyteller. Keep responses to 2-3 paragraphs.",
    })
    e.Prompts().Create(ctx, &loom.Prompt{
        Slug: "assistant-user", Version: 1, Kind: loom.PromptKindUserTemplate,
        Body: "The user says: {{.Action.Payload.text}}",
    })

    sys, _  := e.Prompts().Get(ctx, "assistant-sys", 1)
    user, _ := e.Prompts().Get(ctx, "assistant-user", 1)

    e.Agents().Create(ctx, &loom.Agent{
        Slug: "assistant", Version: 1, Modal: loom.ModalityText,
        GeneratorSlug:  "gpt4o",
        SystemPromptID: sys.ID,
        UserTemplateID: user.ID,
    })

    // Open a session and run steps.
    sess := &loom.Session{PlatformID: "user-1", State: loom.State{Modality: loom.ModalityText}}
    e.Sessions().Create(ctx, sess)

    step, err := e.RunStep(ctx, sess, loom.StepRequest{
        AgentSlug: "assistant",
        Action: &loom.Action{
            Kind:    loom.ActionFreeText,
            Payload: map[string]any{"text": "I push open the ancient door."},
        },
    })
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(step.Result.(*loom.TextResult).Content)
}
```

---

## Core concepts

### Engine

`loom.Engine` is the central object. Construct it once at startup with a `Config`:

```go
e, err := loom.New(loom.Config{
    DB:         db,
    Dialect:    loom.DialectPostgres,
    Generators: map[string]loom.Generator{"openai": gen},
    MaxRetries: 3,
    Logger:     myLogger,
})
```

### Agents & Prompts

An **Agent** bundles a generator slug, system prompt UUID, user template UUID, generation params, and optional response format. Agents are versioned — increment the version to roll out a new prompt or model without breaking existing sessions.

```go
// Prompts are versioned, immutable records.
e.Prompts().Create(ctx, &loom.Prompt{Slug: "my-sys", Version: 1, Kind: loom.PromptKindSystem, Body: "..."})

// Agents reference prompts by UUID.
e.Agents().Create(ctx, &loom.Agent{
    Slug: "my-agent", Version: 1,
    GeneratorSlug:  "openai",
    SystemPromptID: sysPrompt.ID,
    UserTemplateID: userPrompt.ID,
})
```

User templates are Go `text/template` strings. The template data is:

```tmpl
{{.Vars}}            — session.State.Vars (map[string]any)
{{.Action}}          — the current Action
{{.Action.Payload}}  — the action's payload (map[string]any for free-text)
{{.Inputs}}          — per-step inputs (e.g. a turn's lead output; map[string]any)
{{.Params}}          — per-call tuning knobs (map[string]any)
{{.Session}}         — a minimal, whitelisted view: .ID, .PlatformID, .Tags, .State
```

`{{.Session}}` deliberately excludes `Session.Metadata` and `History` so a template
cannot exfiltrate secrets or cross-turn data. `{{define}}`/`{{template}}` actions are
rejected (they enable unbounded recursion).

### Sessions & Steps

A **Session** holds the full conversation history, state variables, and tags. A **Step** is one agent invocation — it records the request, result, action, and duration.

```go
sess := &loom.Session{
    PlatformID: "user-123",
    State: loom.State{
        Modality: loom.ModalityText,
        Vars:     map[string]any{"scene": "A dark tavern"},
    },
}
e.Sessions().Create(ctx, sess)

step, err := e.RunStep(ctx, sess, loom.StepRequest{
    AgentSlug: "assistant",
    Action: &loom.Action{
        Kind:    loom.ActionFreeText,
        Payload: map[string]any{"text": "I order a drink."},
    },
    OnChunk: func(c loom.Chunk) { fmt.Print(c.Content) }, // streaming
})
```

### Turns & multi-agent flows

`RunStep` runs one agent. Many platforms need a *turn* that composes several
agents — e.g. a streaming "author" produces prose, then a "logician", "sensory
director", and "titler" analyse that prose in parallel. A `Flow` declares that
turn as data; `RunTurn` executes it, running the lead first (optionally
streaming), injecting its output into the followers, then running the followers
concurrently. Every resulting step is persisted and tagged with one `turn_id`.

```go
flow := loom.Flow{
    Slug: "chat-turn",
    Lead: loom.FlowAgent{AgentSlug: "author", Stream: true, OutputKey: "Prose"},
    Followers: []loom.FlowAgent{
        {AgentSlug: "logician"}, // sees the lead's prose via {{.Inputs.Prose}}
        {AgentSlug: "sensory"},
    },
}

turn, err := e.RunTurn(ctx, sess, loom.TurnRequest{
    Flow:    flow,
    Action:  &loom.Action{Kind: loom.ActionFreeText, Payload: map[string]any{"text": "I open the door."}},
    OnChunk: func(c loom.Chunk) { fmt.Print(c.Content) }, // streams the lead
})
prose := loom.ResultText(turn.Lead.Result)
logician := turn.Followers["logician"].Result // a *StructuredResult
```

Because every agent in a flow is an ordinary versioned agent, the only thing that
changes between products — or between text, image, video, and spatial/AR
modalities — is the set of agents/prompts the flow references plus the registered
generators. See [examples/conexus-loom](examples/conexus-loom) for a full
multi-agent, multimodal session that runs with no API keys.

#### Per-step inputs, session-aware hooks, provider overrides

- `StepRequest.Inputs` / `TurnRequest.Inputs` are exposed to user templates as
  `{{.Inputs.x}}` and forwarded to the generator. `RunTurn` uses this to pass the
  lead's output to followers.
- Pre-hooks run **before** the template is rendered and receive the session via
  `StepRequest.Session`, so a memory-recall hook can inject context that the
  prompt then uses.
- `StepRequest.GeneratorOverride` / `ParamOverride` / `Overrides` allow
  per-request provider, parameter, and key/model routing (e.g. per-user API keys).
- `Engine.RegisterGenerator(slug, gen)` adds a modality at runtime.

### Branching & Replay

Fork a session at any step index to explore an alternative timeline. The parent session is untouched. Stale branches are cleaned up automatically by the GC worker.

```go
// Fork after step 2 to try a different choice.
branch, err := e.Sessions().Fork(ctx, sess.ID, 2)

// Run a divergent step on the branch.
e.RunStep(ctx, branch, loom.StepRequest{AgentSlug: "assistant", Action: action})

// Inspect the tree.
tree, _ := e.Sessions().BranchTree(ctx, sess.ID)
```

### Generators

Register any number of generators under a slug. Built-in adapters:

| Package | Generator | Modality |
|---|---|---|
| `generator/openai` | `NewChatGenerator(key, model)` | Text (streaming) |
| `generator/anthropic` | `NewChatGenerator(key, model)` | Text (streaming) |
| `generator/replicate` | `NewImageGenerator(key, model)` | Image (async) |
| `generator/runway` | `NewVideoGenerator(key, model)` | Video (async) |
| `generator/echo` | `New(prefix)` | Text — echo stub for testing |

Bring your own by implementing one of three flavors — register it with
`Config.Generators` or `e.RegisterGenerator("slug", gen)` and point an agent at it:

```go
// Sync (the minimum):
type Generator interface {
    Modality() Modality
    Generate(ctx context.Context, req GenerateRequest) (Result, error)
}

// Streaming (optional — word-by-word):
type StreamingGenerator interface {
    Generator
    GenerateStream(ctx context.Context, req GenerateRequest) (<-chan Chunk, <-chan Result, error)
}

// Async (optional — for slow image/video jobs): Generate returns a pending
// Result (NewPendingResult); the engine's background poller calls Poll until it
// resolves. Enable via Config.AsyncPoller + e.StartPoller(ctx).
type AsyncGenerator interface {
    Generator
    Poll(ctx context.Context, handle TaskHandle) (Result, error)
}
```

> **Security note.** The built-in `openai`/`anthropic` generators accept a per-request
> `Overrides["base_url"]` **only** if the URL is allowlisted via `WithAllowedBaseURLs(...)`.
> This is deliberate: an un-allowlisted base URL would let a caller redirect the request —
> and your API key — to an arbitrary host.

See [`llms.txt`](llms.txt) for full, copy-pasteable custom-generator examples.

### Actions

Actions carry structured user input into a step:

| Kind | Payload type | Use case |
|---|---|---|
| `ActionFreeText` | `{"text": "..."}` | Open text input |
| `ActionSelect` | `{"option_index": 0, "option_id": "..."}` | Choose from presented options |
| `ActionSpatial` | `{"direction": "north"}` | Movement / map navigation |
| `ActionInventory` | custom | Item use / equipment |
| `ActionGesture` | custom | Touch / controller input |
| `ActionCustom` | any | Escape hatch |

### Hook bus

Hooks let you intercept every step for validation, retry logic, content moderation, or annotation. They are registered by name and run in registration order.

```go
// Pre-hook: cancel a step if conditions aren't met.
e.Hooks().RegisterPre("check-hp", func(ctx context.Context, req *loom.StepRequest) error {
    if character.CurrentHP <= 0 {
        return loom.ErrSkip // silently skip the step
    }
    return nil
})

// Post-hook: retry if the output contains a forbidden phrase.
e.Hooks().RegisterPost("no-spoilers", func(ctx context.Context, req *loom.StepRequest, res loom.Result) (loom.Result, error) {
    tr := res.(*loom.TextResult)
    if strings.Contains(tr.Content, "THE KILLER IS") {
        return nil, loom.ErrRetryWith(loom.RetryAnnotation{
            Reason: "Do not reveal the killer's identity.",
        })
    }
    return res, nil
})
```

### Error handling

`RunStep` returns typed errors — match them with `errors.Is` / `errors.As`, not
string comparison:

```go
step, err := e.RunStep(ctx, sess, req)
switch {
case err == nil:
    // ok
case loom.IsSkip(err):
    // a pre-hook skipped the step (not a failure)
case errors.Is(err, loom.ErrNotFound):
    var nf *loom.NotFoundError // nf.Kind ("agent"/"prompt"/…), nf.Key
    errors.As(err, &nf)
case errors.Is(err, loom.ErrGeneratorNotRegistered):
    // misconfiguration
default:
    var ge *loom.GenerationError
    if errors.As(err, &ge) && ge.Kind == loom.GenerationTransport {
        // provider transport failure — safe to retry
    }
}
```

Other typed errors: `*loom.BudgetExceededError` (budget block), `*loom.RetryError`
(via `loom.IsRetry` / `loom.RetryAnnotationFrom`), and `loom.ErrInvalidConfig` from
`loom.New`. See [`llms.txt`](llms.txt) for the full contract.

### Cost tracking & budgets

Every step records input/output tokens and USD cost (based on the built-in pricing
table). Budgets enforce a spend cap per platform key (`Target.Kind` must be
`loom.TargetPlatformID` — other kinds are rejected). Enforcement is best-effort
(reads prior recorded spend; fails open on DB errors), not a hard transactional
ceiling.

```go
// Create a daily USD budget for a user.
e.Budgets().Create(ctx, &loom.Budget{
    Name:     "user-daily-cap",
    Target:   loom.BudgetTarget{Kind: loom.TargetPlatformID, Key: "user-123"},
    Window:   loom.BudgetWindowDay,
    Limit:    loom.BudgetLimit{USD: 0.50}, // USD/Tokens/Steps; 0 = no cap on that axis
    OnExceed: loom.BudgetBlock,            // or BudgetDowngrade (to Agent.FallbackAgentID) / BudgetNotify
    Active:   true,                        // required; an inactive budget is ignored
})
// On block, RunStep returns *loom.BudgetExceededError.

// Query cumulative usage.
usage, _ := e.Cost().SessionUsage(ctx, sess.ID)
fmt.Printf("Session cost: $%.4f (%d tokens)\n", usage.TotalUSD, usage.TotalTokens)
```

### Judge subsystem

Score and compare outputs using another LLM as judge:

```go
import "github.com/rhaqim/loom/judge"

rubric := e.Judges().Rubric("quality-judge")
verdict, _ := rubric.Score(ctx, judge.ScoreRequest{
    Input:      userPrompt,
    Output:     dmResponse,
    Dimensions: []string{"immersion", "rules_accuracy", "narrative_coherence"},
})
fmt.Printf("Immersion: %.1f/10\n", verdict.Scores["immersion"])

// Pairwise comparison.
pair := e.Judges().Pairwise("ab-judge")
result, _ := pair.Compare(ctx, judge.PairwiseRequest{
    Input: prompt, OutputA: responseA, OutputB: responseB,
})
// result.Winner == "A" | "B" | "tie"
```

### Test harness

Write test plans in code or YAML. The harness runs all variants in parallel:

```go
import (
    loom "github.com/rhaqim/loom"
    "github.com/rhaqim/loom/harness"
)

plan := &harness.TestPlan{
    Name: "assistant-v2",
    Session: harness.SessionScript{
        PlatformID: "test-user",
        Steps: []harness.ScriptedStep{
            {AgentSlug: "assistant", ActionPayload: "I enter the cave."},
            {AgentSlug: "assistant", ActionPayload: "I search for traps."},
        },
    },
    Variants: harness.VariantMatrix{
        Providers: []string{"openai", "anthropic"},
    },
    Assertions: []harness.Assertion{
        harness.MinLength(80),
        harness.NoKeyword("ERROR"),
        harness.HasStatus(loom.ResultStatusReady),
    },
}

report, err := harness.Run(ctx, e, plan)
fmt.Printf("Passed: %v\n", report.Passed())
```

Or drive it from the CLI:

```bash
loom-cli test my-plan.yaml
```

---

## CLI (`loom-cli`)

```bash
# Apply the schema.
loom-cli migrate

# Seed agents and prompts from a YAML file.
loom-cli seed seed.yaml

# Run a test plan.
loom-cli test plan.yaml
```

Set the DSN via `LOOM_DSN` (preferred) or `--dsn-file <path>`; `--dsn <string>`
also works but exposes credentials in `ps`, so prefer the others.

**Seed file format:**

```yaml
prompts:
  - slug: assistant-sys
    version: 1
    kind: system
    category: chat
    body: "You are a vivid storyteller."

agents:
  - slug: assistant
    version: 1
    modal: text
    generator_slug: openai
```

---

## Docker / local development

```bash
# Start Postgres 16.
make up

# Apply schema.
make migrate

# Seed example data.
make seed

# Run the example app.
make example

# Run all integration tests.
make test-all
```

Requires `DND_DSN` / `LOOM_DSN` set, or the default `postgres://loom:loom@localhost:5432/loom?sslmode=disable`.

---

## Project layout

The public API is a thin **facade**: applications import only `github.com/rhaqim/loom`.
The implementation lives under `internal/engine` (unimportable by other modules), so
its layout can change without breaking you.

```sh
loom/
├── aliases.go          # GENERATED public API — type aliases + re-exports of internal/engine
├── doc.go              # package doc + go:generate directive for the facade
├── llms.txt            # AI-oriented guide (point your AI assistant here)
├── llms-full.txt       # GENERATED: llms.txt + full go doc API reference
├── internal/
│   ├── engine/         # the engine implementation (package engine), grouped by domain:
│   │   ├── api_*.go     #   public types & interfaces (Agent, Prompt, Session, Result, …)
│   │   ├── service_*.go #   registry/service implementations (agent, prompt, session, …)
│   │   ├── store_*.go   #   SQL persistence
│   │   ├── engine.go, flow.go, poller.go, hook.go, cache.go, template.go, …
│   │   └── query.go     #   persistence wrappers (e.g. NotFoundError translation)
│   ├── tools/facadegen/ # generator that emits aliases.go from internal/engine
│   ├── enginetest/      # engine integration tests (skip without LOOM_DSN)
│   └── clitest/         # CLI integration tests (skip without LOOM_DSN)
├── schema/             # idempotent DDL loader + ordered migrations (Postgres + SQLite)
├── generator/
│   ├── openai/         # OpenAI chat completions (sync + streaming)
│   ├── anthropic/      # Anthropic Messages API (sync + streaming)
│   ├── replicate/      # async image generation
│   ├── runway/         # async video generation
│   └── echo/           # echo stub (testing, no API key needed)
├── judge/              # RubricJudge, PairwiseJudge, ConstraintJudge
├── gc/                 # background branch GC worker
├── harness/            # TestPlan, VariantMatrix, Assertion DSL, parallel runner
├── cmd/loom-cli/       # CLI: migrate, seed, test
├── scripts/            # gen-llms-full.sh
├── docs/               # design/architecture notes
└── examples/           # each has its own go.mod
    ├── quickstart/     # smallest possible program
    ├── story/          # minimal single-agent example
    ├── dnd/            # full D&D solo experience
    ├── storyapi/       # HTTP API with auth, sessions, streaming
    └── conexus-loom/   # multi-agent, multimodal session via RunTurn (zero-setup)
```

Contributors: `aliases.go` and `llms-full.txt` are generated — run `make generate`
after changing the public surface (CI enforces the facade is in sync).

---

## License

MIT
