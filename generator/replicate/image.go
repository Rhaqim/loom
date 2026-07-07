// Package replicate provides a Loom generator adapter for Replicate image models.
// It uses the async prediction pattern: Generate submits the job and returns a
// PendingResult; the engine's async poller calls Poll to resolve it.
package replicate

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

const defaultBaseURL = "https://api.replicate.com/v1"

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

// ImageGenerator implements loom.Generator for Replicate image models.
// Generation is asynchronous: Generate returns a PendingResult and Poll
// resolves it once the prediction completes.
type ImageGenerator struct {
	apiKey  string
	model   string // e.g. "stability-ai/sdxl:39ed52f2..."
	baseURL string
	client  *http.Client
}

// NewImageGenerator creates a Replicate image generator.
func NewImageGenerator(apiKey, model string) *ImageGenerator {
	return &ImageGenerator{
		apiKey:  apiKey,
		model:   model,
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *ImageGenerator) Modality() loom.Modality { return loom.ModalityImage }

// Generate submits an image prediction to Replicate and returns a PendingResult.
// The task handle ID is the Replicate prediction ID.
func (g *ImageGenerator) Generate(ctx context.Context, req loom.GenerateRequest) (loom.Result, error) {
	payload := map[string]any{
		"version": g.model,
		"input":   map[string]any{"prompt": req.UserPrompt},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	predID, err := g.createPrediction(ctx, body)
	if err != nil {
		return nil, err
	}

	handle := &loom.TaskHandle{
		ID:       uuid.New(),
		Provider: "replicate",
		Handle:   predID,
	}
	return loom.NewPendingResult(loom.ModalityImage, handle), nil
}

// Poll fetches the prediction status from Replicate and returns the final
// ImageResult once it succeeds (or an error on failure/timeout).
func (g *ImageGenerator) Poll(ctx context.Context, handle loom.TaskHandle) (loom.Result, error) {
	pred, err := g.getPrediction(ctx, handle.Handle)
	if err != nil {
		return nil, err
	}

	switch pred.Status {
	case "succeeded":
		if len(pred.Output) == 0 {
			return nil, fmt.Errorf("replicate: no output URLs")
		}
		return loom.NewImageResult(pred.Output[0], "", 0, 0), nil
	case "failed", "canceled":
		return nil, fmt.Errorf("replicate: prediction %s: %s", pred.ID, pred.Error)
	default:
		// still processing
		return loom.NewPendingResult(loom.ModalityImage, &handle), nil
	}
}

// -----------------------------------------------------------------------
// Internal types and helpers
// -----------------------------------------------------------------------

type prediction struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Output []string `json:"output"`
	Error  string   `json:"error"`
}

func (g *ImageGenerator) createPrediction(ctx context.Context, body []byte) (string, error) {
	resp, err := g.post(ctx, "/predictions", body)
	if err != nil {
		return "", err
	}
	var pred prediction
	if err := json.Unmarshal(resp, &pred); err != nil {
		return "", fmt.Errorf("replicate: parse prediction: %w", err)
	}
	if pred.ID == "" {
		return "", fmt.Errorf("replicate: empty prediction ID")
	}
	return pred.ID, nil
}

func (g *ImageGenerator) getPrediction(ctx context.Context, id string) (*prediction, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/predictions/"+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+g.apiKey)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("replicate: http: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("replicate: http %d: %s", resp.StatusCode, truncErr(b))
	}
	var pred prediction
	if err := json.Unmarshal(b, &pred); err != nil {
		return nil, fmt.Errorf("replicate: parse prediction: %w", err)
	}
	return &pred, nil
}

func (g *ImageGenerator) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+g.apiKey)

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("replicate: http: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("replicate: http %d: %s", resp.StatusCode, truncErr(b))
	}
	return b, nil
}
