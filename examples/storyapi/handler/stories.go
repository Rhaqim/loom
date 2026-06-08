package handler

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/rhaqim/story-api/domain"
)

// GET /api/stories
func (h *Handler) ListStories(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	stories, err := h.store.ListStoriesByUser(r.Context(), user.ID)
	if err != nil {
		h.log.Error("list stories", "user_id", user.ID, "err", err)
		h.fail(w, http.StatusInternalServerError, "could not list stories")
		return
	}
	if stories == nil {
		stories = []*domain.Story{}
	}
	h.ok(w, http.StatusOK, map[string]any{"stories": stories})
}

// GET /api/stories/{id}
func (h *Handler) GetStory(w http.ResponseWriter, r *http.Request) {
	story, ok := h.parseStoryID(w, r)
	if !ok {
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"story": story})
}

// POST /api/stories
func (h *Handler) CreateStory(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var body struct {
		TopicID string `json:"topic_id"`
		Name    string `json:"name"`
	}
	if !h.decodeBody(w, r, &body) {
		return
	}
	if body.TopicID == "" || body.Name == "" {
		h.fail(w, http.StatusBadRequest, "topic_id and name are required")
		return
	}

	topicID, err := uuid.Parse(body.TopicID)
	if err != nil {
		h.fail(w, http.StatusBadRequest, "invalid topic_id")
		return
	}
	topic, err := h.store.GetTopicByID(r.Context(), topicID)
	if err != nil {
		h.fail(w, http.StatusNotFound, "topic not found")
		return
	}

	// Ensure the narrator agent exists in loom (idempotent).
	agentSlug, err := h.engine.SeedStoryAgent(r.Context(), topic)
	if err != nil {
		h.log.Error("seed story agent", "topic", topic.Slug, "err", err)
		h.fail(w, http.StatusInternalServerError, "could not prepare narrator")
		return
	}
	_ = h.store.UpdateTopicAgentSlug(r.Context(), topic.ID, agentSlug)

	story := &domain.Story{
		UserID:  user.ID,
		TopicID: topic.ID,
		Name:    body.Name,
	}
	if err := h.store.CreateStory(r.Context(), story); err != nil {
		h.log.Error("create story", "err", err)
		h.fail(w, http.StatusInternalServerError, "could not create story")
		return
	}

	h.ok(w, http.StatusCreated, map[string]any{"story": story})
}

// DELETE /api/stories/{id}
func (h *Handler) AbandonStory(w http.ResponseWriter, r *http.Request) {
	story, ok := h.parseStoryID(w, r)
	if !ok {
		return
	}
	if err := h.store.AbandonStory(r.Context(), story.ID); err != nil {
		h.log.Error("abandon story", "story_id", story.ID, "err", err)
		h.fail(w, http.StatusInternalServerError, "could not abandon story")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
