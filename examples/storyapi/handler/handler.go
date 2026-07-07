// Package handler implements the HTTP layer for the story-API.
package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/rhaqim/story-api/auth"
	"github.com/rhaqim/story-api/domain"
	"github.com/rhaqim/story-api/game"
	"github.com/rhaqim/story-api/store"
)

type contextKey string

const userCtxKey contextKey = "user"

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	store  *store.Store
	engine *game.Engine
	secret []byte
	log    *slog.Logger
}

// New creates a Handler.
func New(st *store.Store, e *game.Engine, secret []byte, log *slog.Logger) *Handler {
	return &Handler{store: st, engine: e, secret: secret, log: log}
}

// -----------------------------------------------------------------------
// Auth middleware
// -----------------------------------------------------------------------

// protected wraps a HandlerFunc with Bearer-token authentication.
// On success the authenticated *domain.User is placed in request context.
func (h *Handler) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			h.fail(w, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}
		claims, err := auth.ValidateToken(h.secret, token)
		if err != nil {
			// Return a single generic message: distinguishing "expired" from
			// "bad signature" from "bad payload" tells an attacker exactly why a
			// forged/edited token failed.
			h.log.Info("token validation failed", "err", err)
			h.fail(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		user, err := h.store.GetUserByID(r.Context(), claims.UserID)
		if err != nil {
			h.fail(w, http.StatusUnauthorized, "user not found")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next(w, r.WithContext(ctx))
	}
}

// currentUser extracts the authenticated user from the request context.
// Panics if called outside of a protected handler (programming error).
func currentUser(r *http.Request) *domain.User {
	u, ok := r.Context().Value(userCtxKey).(*domain.User)
	if !ok || u == nil {
		panic("currentUser called outside protected handler")
	}
	return u
}

// -----------------------------------------------------------------------
// Response helpers
// -----------------------------------------------------------------------

func (h *Handler) ok(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *Handler) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// maxBodyBytes caps request bodies to prevent memory-exhaustion DoS from an
// unbounded JSON payload (and, for the play endpoints, unbounded LLM input).
const maxBodyBytes = 64 << 10 // 64 KiB

func (h *Handler) decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		h.fail(w, http.StatusBadRequest, "invalid or oversized request body")
		return false
	}
	return true
}

// parseStoryID parses the {id} path value and verifies the story belongs to the user.
func (h *Handler) parseStoryID(w http.ResponseWriter, r *http.Request) (*domain.Story, bool) {
	raw := r.PathValue("id")
	id, err := uuid.Parse(raw)
	if err != nil {
		h.fail(w, http.StatusBadRequest, "invalid story id")
		return nil, false
	}
	story, err := h.store.GetStoryByID(r.Context(), id)
	if err != nil {
		h.fail(w, http.StatusNotFound, "story not found")
		return nil, false
	}
	user := currentUser(r)
	if story.UserID != user.ID {
		h.fail(w, http.StatusForbidden, "story belongs to another user")
		return nil, false
	}
	return story, true
}

// -----------------------------------------------------------------------
// CORS middleware
// -----------------------------------------------------------------------

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------
// Utility
// -----------------------------------------------------------------------

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}
