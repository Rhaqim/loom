# Media Story

A small Go-template web server demonstrating Loom as the engine for a
multi-modal narrative. It contains one deliberately unofficial crossover scene:
a young wizard arrives in the Warhammer 40,000 universe.

Run it without any API keys:

```bash
cd examples/media-story
make run
```

Then open <http://localhost:8080>. The local demo agents exercise text, image,
audio, video, 3D-model, and world-delta result persistence. Their media URLs
are placeholders, so no external generation is charged.

Set `MESHY_API_KEY` before starting the server to add a real Meshy text-to-3D
agent to the 3D picker. The server persists its submitted task and resolves it
through Loom's async poller.

Edit `.env` to set `PORT`, `DB_PATH` (defaults to `db.sqlite`), and optional
`MESHY_API_KEY`. The file-backed SQLite database preserves Loom sessions,
steps, and generated-result records between restarts. Use `make clean` to
remove it and start fresh.
