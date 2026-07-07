package engine

// result_roundtrip_test.go — proves every Result type survives the
// marshal -> store -> unmarshal round trip used by session reload / replay /
// branching. Before the fix, tokens and dimensions were dropped, WorldResult
// deltas vanished, AudioResult round-tripped as a StructuredResult, and a
// StructuredResult's Data came back wrapped in its storage envelope.

import (
	"testing"
)

func roundTrip(t *testing.T, r Result) Result {
	t.Helper()
	payload, err := marshalResultForStorage(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return unmarshalResult(r.Modality(), ResultStatusReady, payload)
}

func TestResultRoundTrip(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		got, ok := roundTrip(t, NewTextResult("hi there", "stop", 7, 11)).(*TextResult)
		if !ok {
			t.Fatal("not a *TextResult")
		}
		if got.Content != "hi there" || got.FinishReason != "stop" || got.InputTokens != 7 || got.OutputTokens != 11 {
			t.Errorf("text round trip lost fields: %+v", got)
		}
	})

	t.Run("image", func(t *testing.T) {
		got, ok := roundTrip(t, NewImageResult("https://x/a.png", "a cat", 512, 768)).(*ImageResult)
		if !ok {
			t.Fatal("not an *ImageResult")
		}
		if got.URL != "https://x/a.png" || got.Prompt != "a cat" || got.Width != 512 || got.Height != 768 {
			t.Errorf("image round trip lost fields: %+v", got)
		}
	})

	t.Run("video", func(t *testing.T) {
		got, ok := roundTrip(t, NewVideoResult("https://x/v.mp4", "https://x/p.png", 5.5, 1280, 720)).(*VideoResult)
		if !ok {
			t.Fatal("not a *VideoResult")
		}
		if got.URL != "https://x/v.mp4" || got.PreviewImage != "https://x/p.png" || got.DurationSec != 5.5 || got.Width != 1280 || got.Height != 720 {
			t.Errorf("video round trip lost fields: %+v", got)
		}
	})

	t.Run("audio", func(t *testing.T) {
		got, ok := roundTrip(t, NewAudioResult("https://x/a.mp3", "mp3", 12.0)).(*AudioResult)
		if !ok {
			t.Fatalf("audio did not round trip as *AudioResult (got %T)", roundTrip(t, NewAudioResult("https://x/a.mp3", "mp3", 12.0)))
		}
		if got.URL != "https://x/a.mp3" || got.Format != "mp3" || got.DurationSec != 12.0 {
			t.Errorf("audio round trip lost fields: %+v", got)
		}
	})

	t.Run("world", func(t *testing.T) {
		src := &WorldResult{Deltas: []WorldDelta{
			{Op: "add_entity", Type: "tree", EntityID: "e1", Pos: [3]float64{1, 2, 3}},
			{Op: "set_lighting", Data: map[string]any{"intensity": 0.8}},
		}}
		src.modal, src.status, src.meta = ModalityWorld, ResultStatusReady, map[string]any{}
		got, ok := roundTrip(t, src).(*WorldResult)
		if !ok {
			t.Fatal("not a *WorldResult")
		}
		if len(got.Deltas) != 2 || got.Deltas[0].EntityID != "e1" || got.Deltas[0].Pos != [3]float64{1, 2, 3} {
			t.Errorf("world round trip lost deltas: %+v", got.Deltas)
		}
	})

	t.Run("structured", func(t *testing.T) {
		got, ok := roundTrip(t, NewStructuredResult(map[string]any{"answer": "yes", "n": float64(3)}, 4, 9)).(*StructuredResult)
		if !ok {
			t.Fatal("not a *StructuredResult")
		}
		if got.Data["answer"] != "yes" || got.Data["n"] != float64(3) {
			t.Errorf("structured Data wrong: %+v", got.Data)
		}
		// Data must be the inner object, not the storage envelope.
		if _, leaked := got.Data["input_tokens"]; leaked {
			t.Errorf("structured Data leaked the storage envelope: %+v", got.Data)
		}
		if got.InputTokens != 4 || got.OutputTokens != 9 {
			t.Errorf("structured tokens lost: in=%d out=%d", got.InputTokens, got.OutputTokens)
		}
	})
}
