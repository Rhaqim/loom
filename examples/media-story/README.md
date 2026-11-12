# Media Story

A small Go-template web server demonstrating Loom as the engine for a
multi-modal narrative, set in an original dark-fantasy world: Wren, a young
lamplighter, walks the drowned cathedral-ship of the Ashen Choir.

Run it without any API keys:

```bash
cd examples/media-story
make run
```

Then open <http://localhost:8080>. The local demo agents exercise text, image,
audio, video, 3D-model, and world-delta result persistence. Their media URLs
are placeholders (the 3D agent loads a public sample GLB into
[`<model-viewer>`](https://modelviewer.dev)), so no external generation is
charged.

Set `MESHY_API_KEY` before starting the server to add a real Meshy text-to-3D
agent to the picker. Its prompts are limited to Meshy's 600 characters and sent
without the story wrapper. The job is persisted as a Loom task and resolved by
the async poller (every 10s, up to 15 minutes per stage); the result page
reloads itself until the model is ready, then renders it in the 3D viewer.
Meshy's asset links are hosted by Meshy and may expire, so a production app
should copy the GLB to its own storage. The cost summary prices each Meshy job
at an assumed $0.40 (`meshyJobUSD`); set it to your plan's real cost.

Edit `.env` to set `PORT`, `DB_PATH` (defaults to `db.sqlite`), and optional
`MESHY_API_KEY`. The file-backed SQLite database preserves Loom sessions,
steps, and generated-result records between restarts; seeding skips agents and
prompts that already exist. Use `make clean` to remove it and start fresh.
