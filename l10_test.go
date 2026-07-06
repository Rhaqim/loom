package loom

// l10_test.go — proves the async resolution seam: OnTaskResolved fires on ready
// and on terminal failure with the owning session/agent, and TaskStatus reports
// pending/ready/failed/not-found without the embedder touching loom_ tables.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func runPendingWithIDs(t *testing.T, ctx context.Context, e *Engine, provider string) (*TaskHandle, uuid.UUID, uuid.UUID) {
	t.Helper()
	agent := &Agent{ID: uuid.New(), Slug: "img-" + uuid.NewString()[:8], Version: 1, Modal: ModalityImage, GeneratorSlug: provider}
	if err := e.Agents().Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityImage}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: agent.Slug})
	if err != nil {
		t.Fatal(err)
	}
	th := step.Result.TaskHandle()
	if th == nil {
		t.Fatalf("want a pending task handle, got status %q", step.Result.Status())
	}
	return th, sess.ID, agent.ID
}

// L10 — a pending task that resolves fires exactly one ready resolution carrying
// the owning session/agent and the resolved asset.
func TestOnTaskResolved_Ready(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "l10_ready", map[string]Generator{"asyncimg": &asyncImageGen{}}, PollerConfig{Interval: time.Minute, Workers: 1})

	var got []TaskResolution
	e.OnTaskResolved(func(_ context.Context, r TaskResolution) { got = append(got, r) })

	th, sessID, agentID := runPendingWithIDs(t, ctx, e, "asyncimg")
	newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1}).pollPending(ctx)

	if len(got) != 1 {
		t.Fatalf("resolutions fired = %d, want exactly 1", len(got))
	}
	r := got[0]
	if r.Status != ResultStatusReady {
		t.Fatalf("status = %v, want ready", r.Status)
	}
	if r.TaskID != th.ID || r.SessionID != sessID || r.AgentID != agentID {
		t.Fatalf("resolution ids = task %v/sess %v/agent %v, want %v/%v/%v", r.TaskID, r.SessionID, r.AgentID, th.ID, sessID, agentID)
	}
	img, ok := r.Result.(*ImageResult)
	if !ok || !strings.Contains(img.URL, "job-1.png") {
		t.Fatalf("resolution result = %#v, want an ImageResult with the resolved URL", r.Result)
	}
}

// L10 — a task that exhausts its poll attempts fires a failed resolution with a
// nil result and a reason.
func TestOnTaskResolved_FailedAtCap(t *testing.T) {
	ctx := context.Background()
	gen := &asyncImageGen{failTimes: 100}
	e, _ := reproEngine(t, "l10_fail", map[string]Generator{"asyncimg": gen}, PollerConfig{Interval: time.Minute, Workers: 1, MaxAttempts: 1})

	var got []TaskResolution
	e.OnTaskResolved(func(_ context.Context, r TaskResolution) { got = append(got, r) })

	th, _, _ := runPendingWithIDs(t, ctx, e, "asyncimg")
	newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1, MaxAttempts: 1}).pollPending(ctx)

	if len(got) != 1 {
		t.Fatalf("resolutions fired = %d, want exactly 1", len(got))
	}
	r := got[0]
	if r.Status != ResultStatusFailed || r.TaskID != th.ID {
		t.Fatalf("resolution = %+v, want a failed resolution for %v", r, th.ID)
	}
	if r.Result != nil {
		t.Fatal("failed resolution should carry a nil Result")
	}
	if r.Err == "" {
		t.Fatal("failed resolution should carry a reason")
	}
}

// L10 — TaskStatus reports each lifecycle state and ErrNotFound for a stranger.
func TestTaskStatus(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "l10_status", map[string]Generator{"asyncimg": &asyncImageGen{}}, PollerConfig{Interval: time.Minute, Workers: 1})
	th, _, _ := runPendingWithIDs(t, ctx, e, "asyncimg")

	status, res, err := e.TaskStatus(ctx, th.ID)
	if err != nil || status != "pending" || res != nil {
		t.Fatalf("before poll: status=%q res=%v err=%v, want pending/nil/nil", status, res, err)
	}

	newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1}).pollPending(ctx)

	status, res, err = e.TaskStatus(ctx, th.ID)
	if err != nil || status != "ready" {
		t.Fatalf("after poll: status=%q err=%v, want ready", status, err)
	}
	if img, ok := res.(*ImageResult); !ok || !strings.Contains(img.URL, "job-1.png") {
		t.Fatalf("ready result = %#v, want the resolved ImageResult", res)
	}

	if _, _, err := e.TaskStatus(ctx, uuid.New()); err != ErrNotFound {
		t.Fatalf("unknown task err = %v, want ErrNotFound", err)
	}
}

// L10 — TaskStatus reports "failed" for a task that was terminally failed.
func TestTaskStatus_Failed(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "l10_status_fail", map[string]Generator{"asyncimg": &asyncImageGen{failTimes: 100}}, PollerConfig{Interval: time.Minute, Workers: 1, MaxAttempts: 1})
	th, _, _ := runPendingWithIDs(t, ctx, e, "asyncimg")

	newAsyncPollerService(e, PollerConfig{Interval: time.Minute, Workers: 1, MaxAttempts: 1}).pollPending(ctx)

	status, res, err := e.TaskStatus(ctx, th.ID)
	if err != nil || status != "failed" {
		t.Fatalf("status=%q err=%v, want failed", status, err)
	}
	if res != nil {
		t.Fatal("failed task should have a nil result")
	}
}
