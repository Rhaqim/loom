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
	"time"

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

	// The token secret must be set explicitly and be long enough to resist
	// forgery. Refuse to boot on a missing/default/weak secret rather than serve
	// traffic with a publicly-known HMAC key (which would let anyone forge a
	// token for any user).
	secret := os.Getenv("TOKEN_SECRET")
	if secret == "" || secret == "change-me-in-production" || len(secret) < 32 {
		log.Error("TOKEN_SECRET must be set to a strong value (>= 32 bytes); refusing to start")
		os.Exit(1)
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

	// Explicit timeouts prevent slow-client (Slowloris) connection exhaustion.
	// WriteTimeout is intentionally omitted because the /turns/stream endpoint
	// streams a long-lived SSE response; ReadHeaderTimeout + IdleTimeout still
	// bound how long an idle or header-dribbling client can hold a connection.
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
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
