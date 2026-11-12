package engine

// async_stage_test.go: regressions for multi-stage async providers (Meshy's
// text-to-3D preview -> refine) and for generators registered under a slug that
// differs from the Provider their adapter writes.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// twoStageGen mimics Meshy: when stage one finishes, Poll starts a separately
// billed stage two and returns a pending result carrying the new handle.
type twoStageGen struct {
	stage2Starts *int
	stage2Polls  *int
	readyAfter   int // stage-two polls before the job resolves
}

func (twoStageGen) Modality() Modality { return ModalityModel3D }
func (twoStageGen) Generate(context.Context, GenerateRequest) (Result, error) {
	// Provider is the adapter's brand, deliberately not the registered slug.
	return NewPendingResult(ModalityModel3D, &TaskHandle{ID: uuid.New(), Provider: "brand", Handle: "stage1:p"}), nil
}
func (g twoStageGen) Poll(_ context.Context, h TaskHandle) (Result, error) {
	switch h.Handle {
	case "stage1:p":
		*g.stage2Starts++
		h.Handle = "stage2:r"
		return NewPendingResult(ModalityModel3D, &h), nil
	case "stage2:r":
		*g.stage2Polls++
		if *g.stage2Polls >= g.readyAfter {
			return NewModel3DResult("https://cdn/x.glb", "model/gltf-binary", "", "x", 0, false), nil
		}
		return NewPendingResult(ModalityModel3D, &h), nil
	}
	return nil, fmt.Errorf("unexpected handle %q", h.Handle)
}

func taskRow(t *testing.T, db *sql.DB, prefix string, id uuid.UUID) (provider, handle string, attempts int) {
	t.Helper()
	if err := db.QueryRow(fmt.Sprintf(`SELECT provider, handle, attempts FROM %stasks WHERE id=$1`, prefix), id).
		Scan(&provider, &handle, &attempts); err != nil {
		t.Fatal(err)
	}
	return
}

func TestAsyncStageAdvanceIsPersisted(t *testing.T) {
	ctx := context.Background()
	starts, polls := 0, 0
	gen := twoStageGen{stage2Starts: &starts, stage2Polls: &polls, readyAfter: 4}
	// Registered as "meshy-3d" while the adapter reports "brand".
	e, db := reproEngine(t, "stage", map[string]Generator{"meshy-3d": gen}, PollerConfig{Interval: time.Minute, Workers: 1})
	th := runPending(t, ctx, e, "meshy-3d", ModalityModel3D)

	if provider, _, _ := taskRow(t, db, e.prefix, th.ID); provider != "meshy-3d" {
		t.Fatalf("task provider = %q, want the registered slug meshy-3d", provider)
	}

	// Stage one already used 3 of a 5-attempt budget. Stage two needs 3 pending
	// polls before resolving, so it only completes if advancing the stage reset
	// the attempt counter.
	if _, err := db.Exec(fmt.Sprintf(`UPDATE %stasks SET attempts=3 WHERE id=$1`, e.prefix), th.ID); err != nil {
		t.Fatal(err)
	}
	p := newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1, MaxAttempts: 5})
	p.pollPending(ctx) // stage one done -> stage two starts
	if _, handle, attempts := taskRow(t, db, e.prefix, th.ID); handle != "stage2:r" || attempts != 0 {
		t.Fatalf("after stage advance: handle=%q attempts=%d, want stage2:r/0", handle, attempts)
	}
	for range 4 {
		p.pollPending(ctx)
	}
	if starts != 1 {
		t.Fatalf("stage two started %d times, want exactly 1 (each start is billed)", starts)
	}
	if got := taskStatus(t, db, e.prefix, th.ID); got != "ready" {
		t.Fatalf("task status = %q, want ready", got)
	}
}
