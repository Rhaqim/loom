package handler

import (
	"net/http"

	"github.com/rhaqim/story-api/domain"
	"github.com/rhaqim/story-api/game"
	"github.com/rhaqim/story-api/store"
)

// GET /api/topics
func (h *Handler) ListTopics(w http.ResponseWriter, r *http.Request) {
	topics, err := h.store.ListTopics(r.Context())
	if err != nil {
		h.log.Error("list topics", "err", err)
		h.fail(w, http.StatusInternalServerError, "could not list topics")
		return
	}
	if topics == nil {
		topics = []*domain.Topic{}
	}
	h.ok(w, http.StatusOK, map[string]any{"topics": topics})
}

// GET /api/topics/{slug}
func (h *Handler) GetTopic(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	topic, err := h.store.GetTopicBySlug(r.Context(), slug)
	if err != nil {
		h.fail(w, http.StatusNotFound, "topic not found")
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"topic": topic})
}

// POST /api/topics
func (h *Handler) CreateTopic(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var body struct {
		Name        string `json:"name"`
		Genre       string `json:"genre"`
		Description string `json:"description"`
	}
	if !h.decodeBody(w, r, &body) {
		return
	}
	if body.Name == "" {
		h.fail(w, http.StatusBadRequest, "name is required")
		return
	}
	if body.Genre == "" {
		body.Genre = "fantasy"
	}
	if !game.ValidGenre(body.Genre) {
		h.fail(w, http.StatusBadRequest, "unsupported genre: "+body.Genre)
		return
	}

	topic := &domain.Topic{
		Slug:        store.Slugify(body.Name),
		Name:        body.Name,
		Genre:       body.Genre,
		Description: body.Description,
		CreatedBy:   &user.ID,
	}
	if err := h.store.CreateTopic(r.Context(), topic); err != nil {
		if isDuplicateErr(err) {
			h.fail(w, http.StatusConflict, "a topic with that name already exists")
			return
		}
		h.log.Error("create topic", "err", err)
		h.fail(w, http.StatusInternalServerError, "could not create topic")
		return
	}

	// Seed the narrator agent in loom for this topic.
	agentSlug, err := h.engine.SeedStoryAgent(r.Context(), topic)
	if err != nil {
		h.log.Error("seed story agent", "topic", topic.Slug, "err", err)
		// Non-fatal: topic is created but agent seeding failed.
	} else {
		_ = h.store.UpdateTopicAgentSlug(r.Context(), topic.ID, agentSlug)
		topic.DMAgentSlug = agentSlug
	}

	h.ok(w, http.StatusCreated, map[string]any{"topic": topic})
}
