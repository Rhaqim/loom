// Command play is the standalone Conexus-on-loom runner. It loads .env, opens a
// persistent SQLite DB (seeding prompts/agents and recording every playthrough),
// migrates block.txt into a world, and plays the story interactively.
//
//	go run ./cmd/play                       # new story from block.txt (latest prompts)
//	go run ./cmd/play -v 2                  # play at prompt version 2
//	go run ./cmd/play -story <uuid>         # resume an existing story
//	CONEXUS_PROVIDER=stub go run ./cmd/play # offline, no API key
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/rhaqim/conexus-loom/adapters"
	"github.com/rhaqim/conexus-loom/engine"
	"github.com/rhaqim/conexus-loom/narrative"
	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/generator/echo"
	"github.com/rhaqim/loom/generator/openai"
	"github.com/rhaqim/loom/schema"
	_ "modernc.org/sqlite"
)

func main() {
	var (
		versionFlag = flag.Int("v", 0, "prompt version to play at (0 = latest)")
		storyFlag   = flag.String("story", "", "resume an existing story id (UUID)")
		blockFlag   = flag.String("block", "block.txt", "path to the block prompt")
		promptsDir  = flag.String("prompts", "narrative/prompts", "directory of versioned prompt files")
		transcript  = flag.String("transcript", "playthrough.md", "investor transcript file (empty to disable)")
		verbose     = flag.Bool("verbose", false, "verbose engine logging")
	)
	flag.Parse()

	_ = adapters.LoadDotEnv(".env")
	ctx := context.Background()
	log := adapters.StdLogger{Verbose: *verbose}

	// Persistent SQLite — prompts/agents/sessions/steps/cost all live here.
	dbPath := adapters.Getenv("CONEXUS_DB", "db.sqlite")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	must(err)
	db.SetMaxOpenConns(1)
	defer db.Close()
	must(schema.NewLoader(schema.DialectSQLite).Apply(ctx, db))

	// Provider selection — the live seam.
	gen, realProvider := pickGenerator(log)

	le, err := loom.New(loom.Config{
		DB:         db,
		Dialect:    loom.DialectSQLite,
		Cache:      loom.NewInProcessCache(),
		Logger:     log,
		Generators: map[string]loom.Generator{narrative.GenText: gen},
		Pricing:    adapters.OpenAIPricing(), // per-model cost; unknown models fall back
	})
	must(err)

	// Seed versioned prompts + agents from files.
	versions, err := narrative.Seed(ctx, le, *promptsDir)
	must(err)
	fmt.Printf("seeded prompt versions: %s\n", narrative.Available(versions))

	storyVersion := *versionFlag
	if storyVersion == 0 {
		storyVersion = narrative.LatestVersion(versions, narrative.AgentAuthor)
	}

	cx := engine.New(engine.Config{
		Loom:            le,
		Memory:          adapters.NewKeywordMemory(),
		Media:           adapters.LogMultimedia{Log: log},
		Plugins:         adapters.NewPluginRegistry(),
		Prefs:           adapters.NoAIPreferences{},
		Log:             log,
		Versions:        versions,
		StoryVersion:    storyVersion,
		ValidateSchemas: realProvider,
	})

	// transcript: optional investor-facing record (see transcript.go).
	tx, err := NewTranscript(*transcript)
	must(err)
	defer tx.Close()

	// Resume or start a story.
	var storyID uuid.UUID
	if *storyFlag != "" {
		storyID, err = uuid.Parse(*storyFlag)
		must(err)
		fmt.Printf("▶ Resuming story %s\n", storyID)
		w, _ := cx.World(ctx, storyID)
		tx.Header(w, storyVersion, true) // transcript
	} else {
		block, rerr := os.ReadFile(*blockFlag)
		must(rerr)
		fmt.Printf("▶ Migrating %s into a world (prompt v%d)...\n", *blockFlag, storyVersion)
		id, w, serr := cx.StartStory(ctx, string(block), "player-1")
		must(serr)
		storyID = id
		fmt.Printf("  World: %q (%s)\n  Story id: %s  (resume with -story %s)\n\n", w.Name, w.Genre, storyID, storyID)
		tx.Header(w, storyVersion, false) // transcript
	}
	if tx != nil {
		fmt.Printf("  📝 transcript → %s\n", tx.Path())
	}

	play(ctx, cx, storyID, tx)
}

func play(ctx context.Context, cx *engine.Conexus, storyID uuid.UUID, tx *Transcript) {
	reader := bufio.NewReader(os.Stdin)
	action := "" // opening turn
	var runTotalUSD float64

	// On resume, present the saved options and take a choice before generating,
	// instead of re-opening the story.
	if snap, err := cx.Snapshot(ctx, storyID); err == nil && snap.Step > 0 && snap.Status == "ONGOING" && len(snap.Options) > 0 {
		fmt.Printf("  Resuming at step %d. Last choices:\n", snap.Step)
		for i, o := range snap.Options {
			fmt.Printf("   %d) %s — %s\n", i+1, o.Label, o.Detail)
		}
		fmt.Print("  > choose 1-3 or type a custom action: ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if isNum(line, len(snap.Options)) {
			n, _ := strconv.Atoi(line)
			action = snap.Options[n-1].Label
		} else if line != "" && line != "q" {
			action = line
		}
	}

	for {
		fmt.Println(strings.Repeat("─", 60))
		fmt.Print("\n  ")
		tr, err := cx.PlayTurn(ctx, storyID, "player-1", action, func(s string) { fmt.Print(s) })
		if err != nil {
			fmt.Println("\n[error]", err)
			return
		}
		runTotalUSD += tr.USD
		fmt.Printf("\n\n  [step %d · %s%s]\n", tr.Step, tr.Status, retried(tr.QARetried))
		if tr.Sensory != nil {
			fmt.Printf("  🎨 %s  [music: %s]\n", tr.Sensory.ImagePrompt, tr.Sensory.MusicMood)
		}
		fmt.Printf("  💸 %d→%d tokens · ~$%.4f this step · ~$%.4f total\n", tr.TokensIn, tr.TokensOut, tr.USD, runTotalUSD)
		tx.Turn(action, tr, runTotalUSD) // transcript

		if tr.Status != "ONGOING" {
			fmt.Printf("\n  THE END — %s\n", tr.Status)
			tx.End(tr.Status, tr.Step, runTotalUSD) // transcript
			fmt.Printf("  💸 final spend: ~$%.4f over %d steps (loom recorded ~$%.4f)\n", runTotalUSD, tr.Step, cx.SessionUSD(ctx, storyID))
			if tx != nil {
				fmt.Printf("  📝 transcript saved → %s\n", tx.Path())
			}
			return
		}

		fmt.Println("\n  What do you do?")
		for i, o := range tr.Options {
			fmt.Printf("   %d) %s — %s\n", i+1, o.Label, o.Detail)
		}
		fmt.Print("  > choose 1-3, type a custom action, 'fork', or 'q': ")

		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		switch {
		case line == "q" || line == "quit":
			fmt.Printf("  (saved — resume with -story %s · run spend ~$%.4f)\n", storyID, runTotalUSD)
			if tx != nil {
				fmt.Printf("  📝 transcript → %s\n", tx.Path())
			}
			return
		case line == "fork":
			bid, ferr := cx.Fork(ctx, storyID)
			if ferr != nil {
				fmt.Println("[fork error]", ferr)
				continue
			}
			fmt.Printf("  forked → playing branch %s (original stays at -story %s)\n", bid, storyID)
			storyID = bid
			action = ""
		case isNum(line, len(tr.Options)):
			n, _ := strconv.Atoi(line)
			action = tr.Options[n-1].Label
		default:
			action = line // free text
		}
	}
}

// pickGenerator returns the text generator and whether it is a real LLM provider
// (real => enable schema validation; the stub's reduced output can't satisfy the
// full v2 schemas).
func pickGenerator(log adapters.StdLogger) (loom.Generator, bool) {
	switch adapters.Getenv("CONEXUS_PROVIDER", "openai") {
	case "stub":
		log.Info("provider", "stub", "note", "offline canned responses")
		return adapters.StubGenerator{}, false
	case "echo":
		return echo.New(""), false
	default:
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" || key == "sk-REPLACE_ME" {
			fmt.Fprintln(os.Stderr, "OPENAI_API_KEY not set in .env — falling back to offline stub provider (set CONEXUS_PROVIDER=stub to silence).")
			return adapters.StubGenerator{}, false
		}
		model := adapters.Getenv("OPENAI_MODEL", "gpt-4o-mini")
		gen := openai.NewChatGenerator(key, model)
		// OpenAI-compatible endpoints: Groq, Together, OpenRouter, DeepSeek,
		// Mistral, local vLLM/Ollama, etc. Set OPENAI_BASE_URL to use them.
		if base := os.Getenv("OPENAI_BASE_URL"); base != "" {
			gen = gen.WithBaseURL(strings.TrimRight(base, "/"))
			log.Info("provider", "openai-compatible", "base_url", base, "model", model)
		}
		return gen, true
	}
}

func isNum(s string, max int) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= max
}

func retried(b bool) string {
	if b {
		return " · QA retried"
	}
	return ""
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
