// Package meshy provides a Loom generator adapter for Meshy's text-to-3D and
// image-to-3D APIs. It produces GLB-backed Model3DResults asynchronously.
package meshy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
)

const defaultBaseURL = "https://api.meshy.ai"

const (
	maxResponseBytes  = 8 << 20
	maxErrorBodyBytes = 4 << 10
)

// Model3DGenerator implements asynchronous Meshy text/image-to-3D generation.
// If req.Params.Extra["image_url"] is a non-empty string, Generate uses Meshy's
// image-to-3D endpoint. Otherwise it starts Meshy's text preview/refine flow.
// Other Extra entries are passed through to Meshy as provider-specific options.
var _ loom.AsyncGenerator = (*Model3DGenerator)(nil)

type Model3DGenerator struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewModel3DGenerator creates a Meshy 3D generator. apiKey is a Meshy API key.
func NewModel3DGenerator(apiKey string) *Model3DGenerator {
	return &Model3DGenerator{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// WithBaseURL replaces Meshy's API URL. It is primarily useful with an
// httptest server; production callers should leave the default unchanged.
func (g *Model3DGenerator) WithBaseURL(url string) *Model3DGenerator {
	clone := *g
	clone.baseURL = strings.TrimRight(url, "/")
	return &clone
}

func (g *Model3DGenerator) Modality() loom.Modality { return loom.ModalityModel3D }

// Generate submits a Meshy job. For image-to-3D, pass the externally reachable
// image URL (or Meshy-supported data URI) in GenerateParams.Extra["image_url"].
func (g *Model3DGenerator) Generate(ctx context.Context, req loom.GenerateRequest) (loom.Result, error) {
	payload := make(map[string]any, len(req.Params.Extra)+2)
	for k, v := range req.Params.Extra {
		if k != "image_url" {
			payload[k] = v
		}
	}

	path, kind := "/openapi/v2/text-to-3d", "text"
	if imageURL, _ := req.Params.Extra["image_url"].(string); imageURL != "" {
		path, kind = "/openapi/v1/image-to-3d", "image"
		payload["image_url"] = imageURL
	} else {
		payload["mode"] = "preview"
		payload["prompt"] = req.UserPrompt
	}

	id, err := g.create(ctx, path, payload)
	if err != nil {
		return nil, err
	}
	return loom.NewPendingResult(loom.ModalityModel3D, &loom.TaskHandle{
		ID: uuid.New(), Provider: "meshy", Handle: kind + ":" + id,
	}), nil
}

// Poll resolves an image task directly. A text task that has completed its
// preview transparently starts the Meshy refine stage, then resolves its GLB.
func (g *Model3DGenerator) Poll(ctx context.Context, handle loom.TaskHandle) (loom.Result, error) {
	kind, id, ok := strings.Cut(handle.Handle, ":")
	if !ok || id == "" || (kind != "text" && kind != "refine" && kind != "image") {
		return nil, fmt.Errorf("meshy: invalid task handle %q", handle.Handle)
	}

	path := "/openapi/v1/image-to-3d/" + id
	if kind == "text" || kind == "refine" {
		path = "/openapi/v2/text-to-3d/" + id
	}
	task, err := g.get(ctx, path)
	if err != nil {
		return nil, err
	}

	switch task.Status {
	case "SUCCEEDED":
		if kind == "text" {
			refineID, err := g.create(ctx, "/openapi/v2/text-to-3d", map[string]any{
				"mode": "refine", "preview_task_id": id,
			})
			if err != nil {
				return nil, err
			}
			handle.Handle = "refine:" + refineID
			return loom.NewPendingResult(loom.ModalityModel3D, &handle), nil
		}
		if task.ModelURLs.GLB == "" {
			return nil, fmt.Errorf("meshy: task %s succeeded without a GLB URL", id)
		}
		return loom.NewModel3DResult(task.ModelURLs.GLB, "model/gltf-binary", task.ThumbnailURL, task.Prompt, 0, false), nil
	case "FAILED", "CANCELED":
		if task.TaskError.Message != "" {
			return nil, fmt.Errorf("meshy: task %s %s: %s", id, strings.ToLower(task.Status), task.TaskError.Message)
		}
		return nil, fmt.Errorf("meshy: task %s %s", id, strings.ToLower(task.Status))
	default:
		return loom.NewPendingResult(loom.ModalityModel3D, &handle), nil
	}
}

type task struct {
	Status       string `json:"status"`
	Prompt       string `json:"prompt"`
	ThumbnailURL string `json:"thumbnail_url"`
	ModelURLs    struct {
		GLB string `json:"glb"`
	} `json:"model_urls"`
	TaskError struct {
		Message string `json:"message"`
	} `json:"task_error"`
}

func (g *Model3DGenerator) create(ctx context.Context, path string, payload map[string]any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("meshy: marshal request: %w", err)
	}
	b, err := g.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return "", err
	}
	var response struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(b, &response); err != nil {
		return "", fmt.Errorf("meshy: parse create response: %w", err)
	}
	if response.Result == "" {
		return "", fmt.Errorf("meshy: empty task ID")
	}
	return response.Result, nil
}

func (g *Model3DGenerator) get(ctx context.Context, path string) (*task, error) {
	b, err := g.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var task task
	if err := json.Unmarshal(b, &task); err != nil {
		return nil, fmt.Errorf("meshy: parse task response: %w", err)
	}
	return &task, nil
}

func (g *Model3DGenerator) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("meshy: http: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 400 {
		if len(b) > maxErrorBodyBytes {
			b = b[:maxErrorBodyBytes]
		}
		return nil, fmt.Errorf("meshy: http %d: %s", resp.StatusCode, b)
	}
	return b, nil
}
