package engine

import (
	"time"

	"github.com/google/uuid"
)

// ResultStatus indicates whether a Result is ready, pending, or failed.
type ResultStatus string

const (
	ResultStatusReady   ResultStatus = "ready"
	ResultStatusPending ResultStatus = "pending"
	ResultStatusFailed  ResultStatus = "failed"
)

// Result is the typed output produced by a Generator.
type Result interface {
	ResultID() uuid.UUID
	Modality() Modality
	Status() ResultStatus
	TaskHandle() *TaskHandle // non-nil for pending async results
	Metadata() map[string]any
}

// baseResult provides common fields shared by all result types.
type baseResult struct {
	ID        uuid.UUID
	modal     Modality
	status    ResultStatus
	task      *TaskHandle
	meta      map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (r *baseResult) ResultID() uuid.UUID      { return r.ID }
func (r *baseResult) Modality() Modality       { return r.modal }
func (r *baseResult) Status() ResultStatus     { return r.status }
func (r *baseResult) TaskHandle() *TaskHandle  { return r.task }
func (r *baseResult) Metadata() map[string]any { return r.meta }

// TextResult is the output from a text-modality generator.
type TextResult struct {
	baseResult
	Content      string
	FinishReason string
	InputTokens  int
	OutputTokens int
}

// ImageResult is the output from an image-modality generator.
type ImageResult struct {
	baseResult
	URL    string // remote URL, data URI, or blob reference
	Width  int
	Height int
	Prompt string
}

// VideoResult is the output from a video-modality generator.
type VideoResult struct {
	baseResult
	URL          string
	DurationSec  float64
	Width        int
	Height       int
	PreviewImage string
}

// AudioResult is the output from an audio-modality generator.
type AudioResult struct {
	baseResult
	URL         string
	DurationSec float64
	Format      string
}

// WorldDelta is a single structured operation applied to a procedural world.
type WorldDelta struct {
	Op       string         `json:"op"`             // "add_entity", "remove_entity", "set_lighting", …
	Type     string         `json:"type,omitempty"` // entity type for add_entity
	EntityID string         `json:"entity_id,omitempty"`
	Pos      [3]float64     `json:"pos,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}

// WorldResult is the output from a world-modality generator.
type WorldResult struct {
	baseResult
	Deltas []WorldDelta
}

// StructuredResult carries arbitrary typed JSON output.
type StructuredResult struct {
	baseResult
	Data         map[string]any
	InputTokens  int
	OutputTokens int
}

// NewTextResult creates a ready TextResult.
func NewTextResult(content, finishReason string, inputTokens, outputTokens int) *TextResult {
	return &TextResult{
		baseResult: baseResult{
			ID:        uuid.New(),
			modal:     ModalityText,
			status:    ResultStatusReady,
			meta:      map[string]any{},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		Content:      content,
		FinishReason: finishReason,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}
}

// NewImageResult creates a ready ImageResult.
func NewImageResult(url, prompt string, width, height int) *ImageResult {
	return &ImageResult{
		baseResult: baseResult{
			ID:        uuid.New(),
			modal:     ModalityImage,
			status:    ResultStatusReady,
			meta:      map[string]any{},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		URL:    url,
		Prompt: prompt,
		Width:  width,
		Height: height,
	}
}

// NewVideoResult creates a ready VideoResult.
func NewVideoResult(url, previewImage string, durationSec float64, width, height int) *VideoResult {
	return &VideoResult{
		baseResult: baseResult{
			ID:        uuid.New(),
			modal:     ModalityVideo,
			status:    ResultStatusReady,
			meta:      map[string]any{},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		URL:          url,
		PreviewImage: previewImage,
		DurationSec:  durationSec,
		Width:        width,
		Height:       height,
	}
}

// NewAudioResult creates a ready AudioResult.
// url is the remote URL or data URI of the audio file.
// format is the MIME type or extension (e.g. "audio/mpeg", "mp3").
// durationSec is the audio length in seconds.
func NewAudioResult(url, format string, durationSec float64) *AudioResult {
	return &AudioResult{
		baseResult: baseResult{
			ID:        uuid.New(),
			modal:     ModalityAudio,
			status:    ResultStatusReady,
			meta:      map[string]any{},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		URL:         url,
		Format:      format,
		DurationSec: durationSec,
	}
}

// NewPendingResult creates a pending Result for async generators.
func NewPendingResult(modality Modality, handle *TaskHandle) Result {
	return &baseResult{
		ID:        uuid.New(),
		modal:     modality,
		status:    ResultStatusPending,
		task:      handle,
		meta:      map[string]any{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

// NewFailedResult creates a failed Result carrying the error message in metadata
// under "error". Streaming generators send this on the result channel to signal a
// provider failure so the engine surfaces it instead of persisting an empty
// successful turn.
func NewFailedResult(modality Modality, err error) Result {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return &baseResult{
		ID:        uuid.New(),
		modal:     modality,
		status:    ResultStatusFailed,
		meta:      map[string]any{"error": msg},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

// NewWorldResult creates a ready WorldResult.
func NewWorldResult(deltas []WorldDelta) *WorldResult {
	return &WorldResult{
		baseResult: baseResult{
			ID:        uuid.New(),
			modal:     ModalityWorld,
			status:    ResultStatusReady,
			meta:      map[string]any{},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		Deltas: deltas,
	}
}

// NewStructuredResult creates a ready StructuredResult.
func NewStructuredResult(data map[string]any, inputTokens, outputTokens int) *StructuredResult {
	return &StructuredResult{
		baseResult: baseResult{
			ID:        uuid.New(),
			modal:     ModalityStructured,
			status:    ResultStatusReady,
			meta:      map[string]any{},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		Data:         data,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}
}
