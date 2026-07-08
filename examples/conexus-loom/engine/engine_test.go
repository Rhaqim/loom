package engine

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/rhaqim/conexus-loom/domain"
	"github.com/rhaqim/conexus-loom/ports"
)

type recQueue struct{ tasks []ports.MediaTask }

func (r *recQueue) Enqueue(_ context.Context, t ports.MediaTask) error {
	r.tasks = append(r.tasks, t)
	return nil
}

func TestEnqueueMediaFansOutAllChannels(t *testing.T) {
	rec := &recQueue{}
	c := &Conexus{cfg: Config{Media: rec}}

	s := &domain.SensoryOutput{
		Image:   domain.ImageDirection{Prompt: "img"},
		Music:   domain.MusicDirection{Mood: "tense"},
		SFX:     domain.SFXDirection{Cue: "snap"},
		Voice:   domain.VoiceDirection{Emotion: "fearful"},
		Ambient: domain.AmbientDirection{Atmosphere: "fog"},
	}
	c.enqueueMedia(context.Background(), uuid.New(), "turn1", "the prose", s)

	kinds := map[ports.MediaKind]bool{}
	for _, tk := range rec.tasks {
		kinds[tk.Kind] = true
	}
	for _, want := range []ports.MediaKind{ports.MediaImage, ports.MediaMusic, ports.MediaSFX, ports.MediaVoice, ports.MediaAmbient} {
		if !kinds[want] {
			t.Errorf("no media task enqueued for kind %q", want)
		}
	}
	if len(rec.tasks) != 5 {
		t.Fatalf("got %d media tasks, want 5", len(rec.tasks))
	}
}

func TestEnqueueMediaSkipsEmptyChannels(t *testing.T) {
	rec := &recQueue{}
	c := &Conexus{cfg: Config{Media: rec}}

	// Only prose present: just the voice (narration) task should fire.
	c.enqueueMedia(context.Background(), uuid.New(), "t", "prose", &domain.SensoryOutput{})
	if len(rec.tasks) != 1 || rec.tasks[0].Kind != ports.MediaVoice {
		t.Fatalf("empty sensory: got %d tasks (%+v), want 1 voice", len(rec.tasks), rec.tasks)
	}

	// Nil sensory enqueues nothing.
	rec2 := &recQueue{}
	c.cfg.Media = rec2
	c.enqueueMedia(context.Background(), uuid.New(), "t", "prose", nil)
	if len(rec2.tasks) != 0 {
		t.Fatalf("nil sensory: got %d tasks, want 0", len(rec2.tasks))
	}
}

func TestEnqueueMediaAmbientColorTintOnly(t *testing.T) {
	rec := &recQueue{}
	c := &Conexus{cfg: Config{Media: rec}}
	// A color_tint/intensity-only ambient direction is schema-valid and must
	// still enqueue (no prose, so no voice task to confuse the count).
	c.enqueueMedia(context.Background(), uuid.New(), "t", "", &domain.SensoryOutput{
		Ambient: domain.AmbientDirection{ColorTint: "#ff8800", Intensity: 0.4},
	})
	if len(rec.tasks) != 1 || rec.tasks[0].Kind != ports.MediaAmbient {
		t.Fatalf("color_tint-only ambient: got %d tasks (%+v), want 1 ambient", len(rec.tasks), rec.tasks)
	}
	if rec.tasks[0].Prompt != "#ff8800" || rec.tasks[0].Params["color_tint"] != "#ff8800" {
		t.Fatalf("ambient task lost the color tint: %+v", rec.tasks[0])
	}
}
