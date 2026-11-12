// Package meshy provides a Loom generator adapter for Meshy's text-to-3D and
// image-to-3D APIs. It produces GLB-backed Model3DResults asynchronously.
package meshy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
)

const defaultBaseURL = "https://api.meshy.ai"

const (
	maxResponseBytes  = 8 << 20
	maxErrorBodyBytes = 4 << 10
)

// MaxPromptChars is Meshy's text-to-3D prompt limit. Longer prompts are
// rejected before submission so the caller gets a clear error instead of a
// provider 400.
const MaxPromptChars = 600

// refinePrefix marks GenerateParams.Extra keys meant for the text-to-3D refine
// stage rather than the preview stage: "refine_enable_pbr": true is sent to
// refine as "enable_pbr": true.
const refinePrefix = "refine_"

// Model3DGenerator implements asynchronous Meshy text/image-to-3D generation.
// If req.Params.Extra["image_url"] is a non-empty string, Generate uses Meshy's
// image-to-3D endpoint. Otherwise it starts Meshy's text preview/refine flow.
// Other Extra entries are passed through to Meshy as provider-specific options;
// in the text flow, keys prefixed "refine_" go to the refine stage instead of
// the preview (prefix stripped), e.g. "refine_enable_pbr" or
// "refine_texture_prompt".
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
	refine := map[string]any{}
	imageURL, _ := req.Params.Extra["image_url"].(string)
	for k, v := range req.Params.Extra {
		switch {
		case k == "image_url":
		case imageURL == "" && strings.HasPrefix(k, refinePrefix):
			refine[strings.TrimPrefix(k, refinePrefix)] = v
		default:
			payload[k] = v
		}
	}

	path, kind := "/openapi/v2/text-to-3d", "text"
	if imageURL != "" {
		path, kind = "/openapi/v1/image-to-3d", "image"
		payload["image_url"] = imageURL
	} else {
		prompt := strings.TrimSpace(req.UserPrompt)
		if prompt == "" {
			return nil, fmt.Errorf("meshy: text-to-3D needs a prompt")
		}
		if n := utf8.RuneCountInString(prompt); n > MaxPromptChars {
			return nil, fmt.Errorf("meshy: prompt is %d characters; Meshy accepts at most %d", n, MaxPromptChars)
		}
		payload["mode"] = "preview"
		payload["prompt"] = prompt
	}

	id, err := g.create(ctx, path, payload)
	if err != nil {
		return nil, err
	}
	handle, err := encodeHandle(kind, id, refine)
	if err != nil {
		return nil, err
	}
	return loom.NewPendingResult(loom.ModalityModel3D, &loom.TaskHandle{
		ID: uuid.New(), Provider: "meshy", Handle: handle,
	}), nil
}

// encodeHandle builds the persisted task handle "kind:id[:opts]", where opts is
// the refine-stage options as base64url JSON. Carrying them in the handle keeps
// them durable: the poller may run in another process, after a restart.
func encodeHandle(kind, id string, refine map[string]any) (string, error) {
	h := kind + ":" + id
	if len(refine) == 0 {
		return h, nil
	}
	b, err := json.Marshal(refine)
	if err != nil {
		return "", fmt.Errorf("meshy: encode refine options: %w", err)
	}
	return h + ":" + base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeHandle(h string) (kind, id string, refine map[string]any, err error) {
	parts := strings.SplitN(h, ":", 3)
	if len(parts) < 2 || parts[1] == "" {
		return "", "", nil, fmt.Errorf("meshy: invalid task handle %q", h)
	}
	kind, id = parts[0], parts[1]
	if kind != "text" && kind != "refine" && kind != "image" {
		return "", "", nil, fmt.Errorf("meshy: invalid task handle %q", h)
	}
	if len(parts) == 3 && parts[2] != "" {
		b, derr := base64.RawURLEncoding.DecodeString(parts[2])
		if derr != nil || json.Unmarshal(b, &refine) != nil {
			return "", "", nil, fmt.Errorf("meshy: invalid refine options in task handle %q", h)
		}
	}
	return kind, id, refine, nil
}

// Poll resolves an image task directly. A text task that has completed its
// preview transparently starts the Meshy refine stage, then resolves its GLB.
func (g *Model3DGenerator) Poll(ctx context.Context, handle loom.TaskHandle) (loom.Result, error) {
	kind, id, refine, err := decodeHandle(handle.Handle)
	if err != nil {
		return nil, err
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
			// The returned handle must be persisted by the poller (Loom does);
			// otherwise the next poll would see the finished preview again and
			// start — and bill — another refine.
			body := make(map[string]any, len(refine)+2)
			for k, v := range refine {
				body[k] = v
			}
			body["mode"], body["preview_task_id"] = "refine", id
			refineID, err := g.create(ctx, "/openapi/v2/text-to-3d", body)
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
