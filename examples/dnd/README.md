# loom-dnd

A solo Dungeons & Dragons experience powered by the [loom](../../) procgen engine.

Play D&D by yourself — with an AI Dungeon Master — to learn the game before joining a group or campaign. You can also *be* the Dungeon Master by describing the world yourself.

---

## How it works

```sh
┌──────────────────────────────────────────────────────────────┐
│                        loom-dnd                              │
│                                                              │
│  dnd_topics ──► dnd_campaigns ──► dnd_characters             │
│       │               │                  │                   │
│       │ (DM agent)    │ (loom session)   │ (session.Vars)    │
│       ▼               ▼                  ▼                   │
│  ┌─────────────────────────────────────────────────────┐     │
│  │                  loom engine                        │     │
│  │  prompt registry · agent registry · session store   │     │
│  │  RunStep → generator (OpenAI / Anthropic / echo)    │     │
│  └─────────────────────────────────────────────────────┘     │
└──────────────────────────────────────────────────────────────┘
```

Each **topic** (setting) gets its own DM agent seeded in loom with a tailored system prompt. Each **campaign** maps 1-to-1 with a loom `Session` — so the DM always has your full history. Your **character sheet** is injected into every turn via `session.State.Vars`, keeping the DM honest about your HP, inventory, and stats.

---

## Prerequisites

- Go 1.22+
- Docker (for PostgreSQL 16) — or any Postgres 16 instance
- An OpenAI or Anthropic API key (optional — `echo` mode works without one for testing)

---

## Quick start

```bash
# 1. Start the database (runs Postgres 16 via Docker from the loom root)
make up

# 2. One-shot setup: migrate + create a topic, user, and campaign
make quickstart

# 3. See the IDs that were created
make list

# 4. Create your character (paste the campaign UUID from step 3)
make character CAMPAIGN_ID=<uuid>

# 5. Play!
make play CAMPAIGN_ID=<uuid>
```

That's it. The echo generator runs without any API key, so you can try the full flow immediately.

---

## Using a real AI Dungeon Master

Export your API key before running any command:

```bash
# OpenAI (gpt-4o)
export OPENAI_API_KEY=sk-...

# Anthropic Claude (preferred — claude-opus-4-5)
export ANTHROPIC_API_KEY=sk-ant-...
```

The engine picks the best available generator automatically (Anthropic > OpenAI > echo). Re-run `make quickstart` or `make migrate` to re-seed the DM agent with the new generator.

---

## Makefile reference

```sh
make up                    Start PostgreSQL 16 via Docker
make down                  Stop PostgreSQL

make build                 Build the loom-dnd binary
make migrate               Apply loom + DND schemas

make quickstart            One-shot setup (migrate + topic + user + campaign)
make list                  List topics, users, and campaigns

make character CAMPAIGN_ID=<uuid>   Create your character (roll stats)
make sheet     CAMPAIGN_ID=<uuid>   Display your character sheet
make play      CAMPAIGN_ID=<uuid>   Start or resume a session

make clean                 Remove built binary
```

All commands accept overrides:

```bash
make quickstart \
  TOPIC_NAME="Ravenloft" \
  SETTING=horror \
  EDITION=5e \
  USERNAME=alice \
  CAMPAIGN="Curse of Strahd"

make character \
  USERNAME=alice \
  CAMPAIGN_ID=<uuid> \
  CHAR_NAME="Mira Darkwood" \
  CHAR_RACE=Half-Elf \
  CHAR_CLASS=Rogue \
  BACKGROUND=Criminal

make play USERNAME=alice CAMPAIGN_ID=<uuid>
```

---

## CLI reference

```bash
# Full form (bypassing Make)
export DND_DSN="postgres://loom:loom@localhost:5432/loom?sslmode=disable"

loom-dnd migrate

loom-dnd topic create --name "Eberron" --setting steampunk --edition 5e
loom-dnd topic list

loom-dnd user create --username thorin
loom-dnd user list

loom-dnd campaign create --topic eberron --name "Shadows of Sharn"
loom-dnd campaign list

loom-dnd character create \
  --username thorin --campaign <uuid> \
  --name "Thorin Ironforge" --race Dwarf --class Fighter \
  --background Soldier --roll-stats

loom-dnd character sheet --username thorin --campaign <uuid>

loom-dnd play --username thorin --campaign <uuid>
```

---

## In-game commands

While playing, type your actions in plain English. Special commands start with `!`:

| Command | Effect |
|---|---|
| `!sheet` | Display your character sheet |
| `!roll 1d20` | Roll dice (any NdS expression) |
| `!damage 8` | Take 8 damage |
| `!heal 6` | Restore 6 HP |
| `!hp 10` | Set current HP to 10 |
| `!gold 50` | Set gold to 50 gp |
| `!item add Rope` | Add an item to inventory |
| `!item drop Torch` | Remove an item from inventory |
| `!help` | List all commands |
| `!quit` | End the session (progress is saved) |

Sessions are persistent — run `make play` again with the same `CAMPAIGN_ID` to resume exactly where you left off.

---

## Available settings

| `--setting` | Flavour |
|---|---|
| `high-fantasy` | Classic D&D — ancient magic, noble quests, epic stakes |
| `dark-fantasy` | Grim, morally ambiguous — magic has a cost |
| `steampunk` | Arcane-industrial age — airships, construct soldiers, guild intrigue |
| `horror` | Survival horror — dread, unreliable NPCs, permanent death |
| `seafaring` | Ocean archipelago — pirates, sea monsters, sunken civilisations |

---

## Project structure

```sh
examples/dnd/
├── main.go             # CLI entry point
├── cmd_migrate.go      # migrate command
├── cmd_topic.go        # topic create/list
├── cmd_user.go         # user create/list
├── cmd_campaign.go     # campaign create/list
├── cmd_character.go    # character create/sheet
├── cmd_play.go         # play REPL
├── db/
│   ├── db.go           # open connection, apply schema
│   └── schema.go       # DDL for dnd_* tables
├── domain/
│   └── types.go        # Topic, User, Campaign, Character types
├── store/
│   └── store.go        # CRUD for all DND tables
└── game/
    ├── dice.go         # dice rolling (d4–d100, RollExpr, AbilityCheck)
    ├── prompts.go      # DM system prompt and user template generators
    └── engine.go       # DndEngine: wraps loom.Engine, seeds agents, runs turns
```
