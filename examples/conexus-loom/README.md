# conexus_refactor

Conexus's interactive-fiction engine, **refactored to run on [loom](../../)**. Same
architecture as the monorepo (migration → multi-agent turn → QA → state → media),
but loom is the underlying engine and every monorepo service is reached through an
**interface (port)** so you can plug the real ones in later. It ships a
**standalone, runnable `main.go`** you can play today — with OpenAI or fully
offline.

## Quick start

```bash
# 1. configure (already created for you; edit the key)
cp .env.example .env        # then set OPENAI_API_KEY

# 2. play the Red Star Belgrade story from block.txt, with OpenAI
go run ./cmd/play

# …or play offline with no API key (canned responses, full pipeline):
CONEXUS_PROVIDER=stub go run ./cmd/play

# resume a story (id is printed at start / on quit):
go run ./cmd/play -story <uuid>

# run the same story on a different prompt version (see "Versioned prompts"):
go run ./cmd/play -v 2

# disable the investor transcript:
go run ./cmd/play -transcript ""
```

Everything persists to **`db.sqlite`**: prompts, agents, sessions, every step,
actions, results, and cost. Delete the file to start clean.

- **Stories** (the prose) live in `loom_results` / `loom_steps` (append-only;
  forks are separate sessions linked by `parent_session_id`).
- **Play state** (location, facts, tension, current options) lives in
  `loom_sessions.state`.
- **Spend** (per-step tokens + USD) lives in `loom_cost_records`.

## Provider compatibility (OpenAI & OpenAI-compatible)

The engine talks plain **chat completions**, so any OpenAI-compatible endpoint
works — Groq, Together, OpenRouter, DeepSeek, Mistral, or a local vLLM/Ollama —
just set the base URL and model in `.env`:

```bash
OPENAI_API_KEY=...
OPENAI_BASE_URL=https://api.groq.com/openai/v1   # omit for api.openai.com
OPENAI_MODEL=llama-3.3-70b-versatile
```

The JSON agents (Logician, Sensory, migration) now use **`response_format`** —
portable `{"type":"json_object"}` by default (set a schema in `seedAgent` for
strict `json_schema`). Parsing tolerates either a structured or text response, so
providers that ignore `response_format` still work via the validating QA retry.
If a provider rejects `response_format` entirely, remove the one marked block in
[narrative/seed.go](narrative/seed.go).

## Transcript & spend report (for investors)

Each turn prints per-step spend (`💸 513→206 tokens · ~$0.0011 this step · ~$X
total`) and appends an investor-friendly Markdown record — prose, choices,
art/music direction, and spend — to `playthrough.md` (`-transcript <path>`, or
`-transcript ""` to disable). loom's own `Cost()` report is shown as a cross-check
at the end. The whole feature is isolated in
[cmd/play/transcript.go](cmd/play/transcript.go) and guarded by nil checks, so you
can drop it later by removing the lines marked `// transcript`.

## How it maps to Conexus (and what changed)

| Conexus monorepo | here | engine |
|---|---|---|
| `StoryOrchestrator.PlayTurn` | [engine/engine.go](engine/engine.go) | `loom.RunTurn` |
| Author / Logician / Sensory `ai.Service` agents | [narrative/](narrative) agents + prompts | loom `Agent` + `Generator` |
| embedded `const` system prompts | **files** in [narrative/prompts/](narrative/prompts) | loom versioned `Prompt` |
| topic → world migration | [migration/](migration) | migrate agent via loom |
| QA validators (dedupe, JSON, option count) | [narrative QAHook](narrative/narrative.go) | loom post-hook + `ErrRetryWith` |
| pgvector memory vault | `ports.MemoryVault` → [adapters.KeywordMemory](adapters/adapters.go) | loom pre-hook injection |
| multimedia worker queue | `ports.MultimediaQueue` → `LogMultimedia` | (app layer) |
| plugins (combat/multiplayer) | `ports.Plugin`/`PluginRegistry` | pre/post-process seams |
| per-user model prefs | `ports.AIPreferences` | per-request override seam |
| telemetry | `ports.Logger` | — |
| `ai.ExecuteWithRetry`, cost, branching | — | **free from loom** |

The difference from the small `conexus-loom` demo: this is a full, layered project
(`domain` / `ports` / `adapters` / `narrative` / `migration` / `engine` / `cmd`),
runs a **real provider**, parses **real LLM JSON** with validating retries, and
persists to a real file DB.

## The seams you plug in (`ports/`)

Each interface in [ports/ports.go](ports/ports.go) has a standalone adapter today
and a one-line swap to the monorepo tomorrow:

- `Logger` — telemetry
- `MemoryVault` — swap keyword recall for the pgvector vault
- `MultimediaQueue` — swap the logger for the real async media worker
- `Plugin` / `PluginRegistry` — combat, multiplayer, governance
- `AIPreferences` — per-account model routing
- `Embedder`, `TopicSource` — embeddings + where block prompts come from

Wiring happens in one place — [cmd/play/main.go](cmd/play/main.go) — so swapping an
adapter never touches the engine.

## Versioned prompts (iterate to find the best)

Prompts live as files named `<agent>-<sys|user>.v<N>.txt` in
[narrative/prompts/](narrative/prompts). On startup every file is seeded into loom
as an immutable, versioned `Prompt`, and one agent version is created per `N`.

To try a new Author prompt for this story:

```bash
# author-*.v2.txt already exists here as a worked example
cp narrative/prompts/author-user.v1.txt narrative/prompts/author-user.v3.txt
$EDITOR narrative/prompts/author-user.v3.txt   # tweak it
go run ./cmd/play -v 3                          # play the story on v3
```

Version resolution is **per agent**: `-v 3` uses the highest available version
**≤ 3** for *each* agent, so bumping only the Author to v3 leaves the Logician and
Sensory on v1. Each playthrough is tagged with its prompt version, so you can
compare runs. (Editing a file but keeping the same version is allowed during
iteration; to change a *shipped* version cleanly, add a new `vN`.)

**v2 ships the real Conexus prompts.** `author-sys.v2.txt`, `logician-sys.v2.txt`,
and `sensory-sys.v2.txt` are the actual production system prompts; `-v 2` (the
default) plays the story on them, `-v 1` on the compact demo prompts.

### Response formats live with the prompt

A system prompt can carry its output JSON schema. Drop a file named
`<agent>-schema.v<N>.json` next to the prompt and the seeder attaches it to that
system prompt and **stores it in the DB alongside the prompt** (loom's new
`Prompt.ResponseFormat`). When the agent runs, loom applies the prompt's schema as
the provider `response_format` — the agent itself configures nothing. So adding a
response format is just: write the system prompt, paste the JSON schema beside it.

- `logician-schema.v2.json` — the real `LogicianAnalyzeSchema`
- `sensory-schema.v2.json` — the real `SensoryDirectSchema`
- JSON agents with no schema file get portable `json_object` mode; free-form
  agents (the Author) get none.

```bash
# inspect what's stored with each system prompt
sqlite3 db.sqlite "select slug,version,length(response_format) from loom_prompts where kind='system';"
```

Parsing tolerates both the compact v1 shape and the real nested v2 schema (e.g.
sensory `image.prompt` / `music.mood`), so either version runs offline or live.

## A turn, end to end

1. **Migration** (first run): the migrate agent turns `block.txt` into a `World` +
   `Constitution`, stored in the loom session.
2. **PlayTurn** → `loom.RunTurn(Flow)`:
   - **pre-hook** recalls relevant facts from memory into the turn,
   - **Author** (lead) streams prose,
   - **Logician** + **Sensory** (followers) analyse it in parallel,
   - **QA post-hook** validates the Logician's JSON/options and makes loom retry
     the agent on failure (you'll see `· QA retried`),
   - the state patch is applied, facts are stored to memory, the image prompt is
     enqueued to the media queue, and the session is saved.
3. Pick an option (or type a custom action), `fork` to branch, or `q` to quit.

## Cross-agent communication, params, validation, pricing

Recent loom additions, all wired here:

- **Cross-agent channels (`Bus`).** Every turn has a channel fabric on
  `GenerateRequest.Bus`; agents (and hooks, via `req.Bus()`) `Publish`/`Subscribe`
  by topic. The lead's chunks stream on the `"lead"` topic and replay, so a
  follower that starts later still receives them. All traffic is on `Turn.Messages`.
- **Params as a map.** `TurnRequest.Params` / `FlowAgent.Params` carry both model
  knobs and domain knobs in one `map[string]any`; known model keys (temperature,
  max_tokens, …) fold onto the typed params, the rest reach the generator
  (`ParamsMap`) and templates (`{{.Params.x}}`). Here: per-agent temperature
  (author 0.85, logician 0.2, sensory 0.5) + turn-wide `tension`.
- **Output schema validation.** With a real provider (`ValidateSchemas`), loom's
  `SchemaValidationPostHook` validates each agent's JSON against the schema stored
  on its prompt and auto-retries on violation. Off for the stub (its reduced
  output intentionally doesn't satisfy the full v2 schemas).
- **Per-model pricing.** `loom.Config.Pricing` (see `adapters.OpenAIPricing`)
  prices each step by the model the generator reports, so the spend report is
  accurate per model; unknown models fall back to the default.

## Notes

- The OpenAI path uses streaming for the Author (loom's SSE streaming was fixed as
  part of this work) and sync JSON for the Logician/Sensory/migration, parsed with
  fence-stripping and a validating retry — robust across `gpt-4o-mini` and up.
- `db.sqlite`, `.env` are git-ignored.
