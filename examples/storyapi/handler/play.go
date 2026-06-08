package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rhaqim/story-api/game"
)

// POST /api/stories/{id}/turns
// Synchronous: waits for the full narrator response before returning.
func (h *Handler) Play(w http.ResponseWriter, r *http.Request) {
	story, ok := h.parseStoryID(w, r)
	if !ok {
		return
	}
	if !story.IsActive() {
		h.fail(w, http.StatusConflict, "story is "+story.Status)
		return
	}

	var body struct {
		Action string `json:"action"`
	}
	if !h.decodeBody(w, r, &body) {
		return
	}
	if body.Action == "" {
		h.fail(w, http.StatusBadRequest, "action is required")
		return
	}

	ctx := r.Context()
	topic, err := h.store.GetTopicByID(ctx, story.TopicID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not load topic")
		return
	}

	agentSlug, err := h.engine.SeedStoryAgent(ctx, topic)
	if err != nil {
		h.log.Error("seed agent", "err", err)
		h.fail(w, http.StatusInternalServerError, "could not prepare narrator")
		return
	}

	sess, err := h.engine.StartSession(ctx, story, currentUser(r).ID.String())
	if err != nil {
		h.log.Error("start session", "err", err)
		h.fail(w, http.StatusInternalServerError, "could not start session")
		return
	}

	result, err := h.engine.Play(ctx, sess, agentSlug, body.Action)
	if err != nil {
		h.log.Error("play", "story_id", story.ID, "err", err)
		h.fail(w, http.StatusInternalServerError, "narrator error: "+err.Error())
		return
	}

	story.TurnCount++
	_ = h.store.UpdateStory(ctx, story)

	h.ok(w, http.StatusCreated, map[string]any{
		"turn_index": result.TurnIndex,
		"response":   result.Response,
		"turn_count": story.TurnCount,
	})
}

// POST /api/stories/{id}/turns/stream
// Streams the narrator response as Server-Sent Events.
//
// SSE event format:
//
//	event: chunk
//	data: {"content":"..."}
//
//	event: done
//	data: {"turn_index":N,"full_response":"...","turn_count":N}
//
//	event: error
//	data: {"error":"..."}
func (h *Handler) PlayStream(w http.ResponseWriter, r *http.Request) {
	story, ok := h.parseStoryID(w, r)
	if !ok {
		return
	}
	if !story.IsActive() {
		h.fail(w, http.StatusConflict, "story is "+story.Status)
		return
	}

	var body struct {
		Action string `json:"action"`
	}
	if !h.decodeBody(w, r, &body) {
		return
	}
	if body.Action == "" {
		h.fail(w, http.StatusBadRequest, "action is required")
		return
	}

	ctx := r.Context()
	topic, err := h.store.GetTopicByID(ctx, story.TopicID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "could not load topic")
		return
	}

	agentSlug, err := h.engine.SeedStoryAgent(ctx, topic)
	if err != nil {
		h.log.Error("seed agent", "err", err)
		h.fail(w, http.StatusInternalServerError, "could not prepare narrator")
		return
	}

	sess, err := h.engine.StartSession(ctx, story, currentUser(r).ID.String())
	if err != nil {
		h.log.Error("start session", "err", err)
		h.fail(w, http.StatusInternalServerError, "could not start session")
		return
	}

	// Switch to SSE mode.
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.fail(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)

	chunks, resultCh := h.engine.PlayStream(ctx, sess, agentSlug, body.Action)

	// Stream chunks.
	for chunk := range chunks {
		writeSSE(w, flusher, "chunk", map[string]any{"content": chunk.Content})
	}

	// Final result.
	result := <-resultCh
	if result.Err != nil {
		h.log.Error("play stream", "story_id", story.ID, "err", result.Err)
		writeSSE(w, flusher, "error", map[string]any{"error": result.Err.Error()})
		return
	}

	story.TurnCount++
	_ = h.store.UpdateStory(ctx, story)

	writeSSE(w, flusher, "done", map[string]any{
		"turn_index":    result.TurnIndex,
		"full_response": result.Response,
		"turn_count":    story.TurnCount,
	})
}

// GET /api/stories/{id}/turns
// Returns the full conversation history for a story.
func (h *Handler) ListTurns(w http.ResponseWriter, r *http.Request) {
	story, ok := h.parseStoryID(w, r)
	if !ok {
		return
	}
	if story.LoomSessionID == "" {
		h.ok(w, http.StatusOK, map[string]any{"turns": []any{}})
		return
	}

	sess, err := h.engine.StartSession(r.Context(), story, currentUser(r).ID.String())
	if err != nil {
		h.log.Error("load session for turns", "err", err)
		h.fail(w, http.StatusInternalServerError, "could not load story history")
		return
	}

	turns := game.TurnsFromSession(sess)
	h.ok(w, http.StatusOK, map[string]any{"turns": turns})
}

// -----------------------------------------------------------------------
// SSE helper
// -----------------------------------------------------------------------

func writeSSE(w http.ResponseWriter, f http.Flusher, event string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
	f.Flush()
}
