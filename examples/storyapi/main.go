// Command story-api is a REST backend for an AI-powered story generator
// built on the loom procgen engine.
//
// Environment variables:
//
//	API_DSN        PostgreSQL connection string (required)
//	PORT           HTTP port (default: 8080)
//	TOKEN_SECRET   HMAC-SHA256 signing secret (default: change-me-in-production)
//	OPENAI_API_KEY     OpenAI API key (optional)
//	ANTHROPIC_API_KEY  Anthropic API key (optional)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	_ "github.com/lib/pq"
	"github.com/rhaqim/story-api/db"
	"github.com/rhaqim/story-api/game"
	"github.com/rhaqim/story-api/handler"
	"github.com/rhaqim/story-api/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dsn := getenv("API_DSN", "postgres://loom:loom@localhost:5432/loom?sslmode=disable")
	port := getenv("PORT", "8080")
	secret := getenv("TOKEN_SECRET", "change-me-in-production")

	if secret == "change-me-in-production" {
		log.Warn("TOKEN_SECRET is using the default insecure value — set TOKEN_SECRET in production")
	}

	conn, err := db.Open(dsn)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	ctx := context.Background()

	log.Info("applying schemas")
	if err := game.ApplyLoomSchema(ctx, conn); err != nil {
		log.Error("loom schema", "err", err)
		os.Exit(1)
	}
	if err := db.ApplySchema(ctx, conn); err != nil {
		log.Error("story schema", "err", err)
		os.Exit(1)
	}

	st := store.New(conn)
	e, err := game.New(ctx, conn, st)
	if err != nil {
		log.Error("game engine", "err", err)
		os.Exit(1)
	}

	log.Info("seeding default topics")
	if err := game.SeedDefaultTopics(ctx, st, e); err != nil {
		log.Warn("seed default topics", "err", err)
	}

	h := handler.New(st, e, []byte(secret), log)
	mux := handler.Routes(h)

	addr := fmt.Sprintf(":%s", port)
	log.Info("server starting",
		"addr", addr,
		"generator", game.BestGeneratorSlug(),
	)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
