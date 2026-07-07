// Package runway provides a Loom generator adapter for RunwayML video models.
// Generation is asynchronous: Generate submits the task and returns a
// PendingResult; Poll resolves it when the video is ready.
package runway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
)

const defaultBaseURL = "https://api.dev.runwayml.com/v1"

const (
	// maxResponseBytes caps a response body so a malicious or misbehaving
	// upstream cannot exhaust memory with a huge reply.
	maxResponseBytes = 8 << 20 // 8 MiB
	// maxErrorBodyBytes truncates an upstream error body before it is echoed
	// into an error string (and thus logs).
	maxErrorBodyBytes = 4 << 10 // 4 KiB
)

// truncErr caps an upstream body slice for safe inclusion in an error message.
func truncErr(b []byte) []byte {
	if len(b) > maxErrorBodyBytes {
		return b[:maxErrorBodyBytes]
	}
	return b
}

// VideoGenerator implements loom.Generator for RunwayML Gen-3 video models.
type VideoGenerator struct {
	apiKey  string
	model   string // e.g. "gen3a_turbo"
	baseURL string
	client  *http.Client
}

// NewVideoGenerator creates a RunwayML video generator.
func NewVideoGenerator(apiKey, model string) *VideoGenerator {
	return &VideoGenerator{
		apiKey:  apiKey,
		model:   model,
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *VideoGenerator) Modality() loom.Modality { return loom.ModalityVideo }

// Generate submits a video generation task to RunwayML and returns a PendingResult.
func (g *VideoGenerator) Generate(ctx context.Context, req loom.GenerateRequest) (loom.Result, error) {
	payload := map[string]any{
		"model":      g.model,
		"promptText": req.UserPrompt,
		"duration":   5,
		"ratio":      "1280:720",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	taskID, err := g.createTask(ctx, body)
	if err != nil {
		return nil, err
	}

	handle := &loom.TaskHandle{
		ID:       uuid.New(),
		Provider: "runway",
		Handle:   taskID,
	}
	return loom.NewPendingResult(loom.ModalityVideo, handle), nil
}

// Poll checks the RunwayML task status and returns the final VideoResult once
// the video is ready.
func (g *VideoGenerator) Poll(ctx context.Context, handle loom.TaskHandle) (loom.Result, error) {
	task, err := g.getTask(ctx, handle.Handle)
	if err != nil {
		return nil, err
	}

	switch task.Status {
	case "SUCCEEDED":
		if len(task.Output) == 0 {
			return nil, fmt.Errorf("runway: no output URLs")
		}
		return loom.NewVideoResult(task.Output[0], "", 0, 0, 0), nil
	case "FAILED":
		return nil, fmt.Errorf("runway: task %s failed: %s", task.ID, task.Failure)
	default:
		// PENDING, RUNNING, THROTTLED
		return loom.NewPendingResult(loom.ModalityVideo, &handle), nil
	}
}

// -----------------------------------------------------------------------
// Internal types and helpers
// -----------------------------------------------------------------------

type runwayTask struct {
	ID      string   `json:"id"`
	Status  string   `json:"status"`
	Output  []string `json:"output"`
	Failure string   `json:"failure"`
}

func (g *VideoGenerator) createTask(ctx context.Context, body []byte) (string, error) {
	resp, err := g.post(ctx, "/tasks", body)
	if err != nil {
		return "", err
	}
	var task runwayTask
	if err := json.Unmarshal(resp, &task); err != nil {
		return "", fmt.Errorf("runway: parse task: %w", err)
	}
	if task.ID == "" {
		return "", fmt.Errorf("runway: empty task ID")
	}
	return task.ID, nil
}

func (g *VideoGenerator) getTask(ctx context.Context, id string) (*runwayTask, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/tasks/"+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	req.Header.Set("X-Runway-Version", "2024-11-06")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runway: http: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("runway: http %d: %s", resp.StatusCode, truncErr(b))
	}
	var task runwayTask
	if err := json.Unmarshal(b, &task); err != nil {
		return nil, fmt.Errorf("runway: parse task: %w", err)
	}
	return &task, nil
}

func (g *VideoGenerator) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	req.Header.Set("X-Runway-Version", "2024-11-06")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runway: http: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("runway: http %d: %s", resp.StatusCode, truncErr(b))
	}
	return b, nil
}
