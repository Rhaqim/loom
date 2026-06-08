package handler

import "net/http"

// Routes registers all API routes and returns the root mux.
// Uses Go 1.22 enhanced ServeMux patterns (method + path).
func Routes(h *Handler) http.Handler {
	mux := http.NewServeMux()

	// Auth
	mux.HandleFunc("POST /api/auth/register", h.Register)
	mux.HandleFunc("POST /api/auth/login", h.Login)
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
	mux.HandleFunc("POST /api/stories/{id}/turns", h.protected(h.Play))
	mux.HandleFunc("POST /api/stories/{id}/turns/stream", h.protected(h.PlayStream))
	mux.HandleFunc("GET /api/stories/{id}/turns", h.protected(h.ListTurns))

	// Health
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	return corsMiddleware(mux)
}
