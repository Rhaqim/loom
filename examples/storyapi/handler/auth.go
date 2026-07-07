package handler

import (
	"net/http"

	"github.com/rhaqim/story-api/auth"
	"github.com/rhaqim/story-api/domain"
)

// POST /api/auth/register
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !h.decodeBody(w, r, &body) {
		return
	}
	if len(body.Username) < 3 {
		h.fail(w, http.StatusBadRequest, "username must be at least 3 characters")
		return
	}
	if len(body.Password) < 8 {
		h.fail(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not hash password")
		return
	}

	user := &domain.User{Username: body.Username, PasswordHash: hash}
	if err := h.store.CreateUser(r.Context(), user); err != nil {
		if isDuplicateErr(err) {
			h.fail(w, http.StatusConflict, "username already taken")
			return
		}
		h.log.Error("create user", "err", err)
		h.fail(w, http.StatusInternalServerError, "could not create user")
		return
	}

	token, err := auth.CreateToken(h.secret, user.ID, user.Username)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not create token")
		return
	}

	h.ok(w, http.StatusCreated, map[string]any{"token": token, "user": user})
}

// POST /api/auth/login
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !h.decodeBody(w, r, &body) {
		return
	}

	user, err := h.store.GetUserByUsername(r.Context(), body.Username)
	if err != nil {
		// Spend bcrypt-equivalent time even when the user does not exist, so
		// response latency cannot be used to enumerate valid usernames.
		auth.DummyCompare(body.Password)
		h.fail(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if err := auth.CheckPassword(user.PasswordHash, body.Password); err != nil {
		h.fail(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, err := auth.CreateToken(h.secret, user.ID, user.Username)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not create token")
		return
	}

	h.ok(w, http.StatusOK, map[string]any{"token": token, "user": user})
}

// GET /api/auth/me
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	h.ok(w, http.StatusOK, map[string]any{"user": currentUser(r)})
}

func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "duplicate key") || contains(s, "UNIQUE constraint failed")
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
