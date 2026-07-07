package loom

import (
	"encoding/json"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id.String()
}

func nullableUUID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

func nullableUUID2(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id.String()
}

func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

func nullInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

// marshalResultForStorage serialises a Result to JSON for loom_results.payload.
func marshalResultForStorage(r Result) ([]byte, error) {
	switch v := r.(type) {
	case *TextResult:
		return json.Marshal(map[string]any{
			"content":       v.Content,
			"finish_reason": v.FinishReason,
			"input_tokens":  v.InputTokens,
			"output_tokens": v.OutputTokens,
		})
	case *ImageResult:
		return json.Marshal(map[string]any{
			"url":    v.URL,
			"width":  v.Width,
			"height": v.Height,
			"prompt": v.Prompt,
		})
	case *VideoResult:
		return json.Marshal(map[string]any{
			"url":           v.URL,
			"duration_sec":  v.DurationSec,
			"width":         v.Width,
			"height":        v.Height,
			"preview_image": v.PreviewImage,
		})
	case *AudioResult:
		return json.Marshal(map[string]any{
			"url":          v.URL,
			"duration_sec": v.DurationSec,
			"format":       v.Format,
		})
	case *WorldResult:
		return json.Marshal(map[string]any{"deltas": v.Deltas})
	case *StructuredResult:
		return json.Marshal(map[string]any{
			"data":          v.Data,
			"input_tokens":  v.InputTokens,
			"output_tokens": v.OutputTokens,
		})
	default:
		return json.Marshal(map[string]any{"modality": string(r.Modality())})
	}
}

// unmarshalResult deserialises a stored payload back to a Result.
func unmarshalResult(modal Modality, status ResultStatus, payload []byte) Result {
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	str := func(k string) string { s, _ := m[k].(string); return s }
	num := func(k string) int { f, _ := m[k].(float64); return int(f) }
	fnum := func(k string) float64 { f, _ := m[k].(float64); return f }
	switch modal {
	case ModalityText:
		r := &TextResult{Content: str("content"), FinishReason: str("finish_reason"), InputTokens: num("input_tokens"), OutputTokens: num("output_tokens")}
		r.modal, r.status, r.meta = ModalityText, status, map[string]any{}
		return r
	case ModalityImage:
		r := &ImageResult{URL: str("url"), Width: num("width"), Height: num("height"), Prompt: str("prompt")}
		r.modal, r.status, r.meta = ModalityImage, status, map[string]any{}
		return r
	case ModalityVideo:
		r := &VideoResult{URL: str("url"), DurationSec: fnum("duration_sec"), Width: num("width"), Height: num("height"), PreviewImage: str("preview_image")}
		r.modal, r.status, r.meta = ModalityVideo, status, map[string]any{}
		return r
	case ModalityAudio:
		r := &AudioResult{URL: str("url"), DurationSec: fnum("duration_sec"), Format: str("format")}
		r.modal, r.status, r.meta = ModalityAudio, status, map[string]any{}
		return r
	case ModalityWorld:
		r := &WorldResult{}
		r.modal, r.status, r.meta = ModalityWorld, status, map[string]any{}
		// Re-decode the deltas through JSON into the typed slice (they were
		// dropped entirely before, losing all world/spatial output on reload).
		if raw, ok := m["deltas"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				_ = json.Unmarshal(b, &r.Deltas)
			}
		}
		return r
	case ModalityStructured:
		r := &StructuredResult{InputTokens: num("input_tokens"), OutputTokens: num("output_tokens")}
		r.modal, r.status, r.meta = ModalityStructured, status, map[string]any{}
		// Read the inner data object; the marshaled payload wraps it as
		// {"data":..., "input_tokens":..., "output_tokens":...}.
		if data, ok := m["data"].(map[string]any); ok {
			r.Data = data
		} else {
			r.Data = m
		}
		return r
	default:
		r := &StructuredResult{}
		r.modal, r.status, r.meta = modal, status, map[string]any{}
		r.Data = m
		return r
	}
}
