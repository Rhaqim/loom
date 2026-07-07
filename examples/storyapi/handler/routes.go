package handler

import (
	"net/http"
	"time"
)

// Routes registers all API routes and returns the root mux.
// Uses Go 1.22 enhanced ServeMux patterns (method + path).
func Routes(h *Handler) http.Handler {
	mux := http.NewServeMux()

	// Per-IP throttle on the unauthenticated auth endpoints to blunt
	// brute-force / credential-stuffing / enumeration.
	authLimit := newRateLimiter(10, time.Minute)

	// Per-IP throttle on the play endpoints: each turn triggers a paid LLM call,
	// so cap the rate to blunt cost-amplification abuse. (loom budgets are the
	// per-user hard cap; this is a coarse first line of defence.)
	playLimit := newRateLimiter(30, time.Minute)

	// Auth
	mux.HandleFunc("POST /api/auth/register", authLimit.middleware(h, h.Register))
	mux.HandleFunc("POST /api/auth/login", authLimit.middleware(h, h.Login))
	mux.HandleFunc("GET /api/auth/me", h.protected(h.Me))

	// Topics
	mux.HandleFunc("GET /api/topics", h.ListTopics)
	mux.HandleFunc("POST /api/topics", h.protected(h.CreateTopic))
	mux.HandleFunc("GET /api/topics/{slug}", h.GetTopic)

	// Stories
	mux.HandleFunc("GET /api/stories", h.protected(h.ListStories))
	mux.HandleFunc("POST /api/stories", h.protected(h.CreateStory))
	mux.HandleFunc("GET /api/stories/{id}", h.protected(h.GetStory))
	mux.HandleFunc("DELETE /api/stories/{id}", h.protected(h.AbandonStory))

	// Play
	mux.HandleFunc("POST /api/stories/{id}/turns", playLimit.middleware(h, h.protected(h.Play)))
	mux.HandleFunc("POST /api/stories/{id}/turns/stream", playLimit.middleware(h, h.protected(h.PlayStream)))
	mux.HandleFunc("GET /api/stories/{id}/turns", h.protected(h.ListTurns))

	// Health
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	return corsMiddleware(mux)
}
