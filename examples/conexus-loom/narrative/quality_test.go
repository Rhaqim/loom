package narrative

import (
	"context"
	"testing"

	loom "github.com/rhaqim/loom"
)

func authorReq(lastProse string) *loom.StepRequest {
	return &loom.StepRequest{
		AgentSlug: AgentAuthor,
		Session:   &loom.Session{State: loom.State{Vars: map[string]any{"last_prose": lastProse}}},
	}
}

func TestSlopBanHook(t *testing.T) {
	hook := SlopBanHook()
	ctx := context.Background()

	// Clean prose passes through.
	clean := loom.NewTextResult("She crossed the bridge and the city opened below her.", "stop", 1, 1)
	if _, err := hook(ctx, authorReq(""), clean); err != nil {
		t.Fatalf("clean prose should pass: %v", err)
	}

	// Cliché prose triggers a retry carrying the offending phrase.
	slop := loom.NewTextResult("A chill ran down her spine as little did she know what waited.", "stop", 1, 1)
	_, err := hook(ctx, authorReq(""), slop)
	if !loom.IsRetry(err) {
		t.Fatalf("slop prose should trigger a retry, got %v", err)
	}
	if ann, ok := loom.RetryAnnotationFrom(err); !ok || len(ann.Forbidden) == 0 {
		t.Fatalf("retry should list forbidden phrases, got %+v", ann)
	}

	// Non-author agents are untouched even with slop.
	req := authorReq("")
	req.AgentSlug = AgentLogician
	if _, err := hook(ctx, req, slop); err != nil {
		t.Fatalf("non-author should pass through: %v", err)
	}
}

func TestStalenessHook(t *testing.T) {
	hook := StalenessHook(0)
	ctx := context.Background()
	scene := "She walked into the dark room slowly and the floor creaked beneath her."

	// Near-identical to the previous turn → retry.
	if _, err := hook(ctx, authorReq(scene), loom.NewTextResult(scene, "stop", 1, 1)); !loom.IsRetry(err) {
		t.Fatalf("repeated scene should trigger a retry, got %v", err)
	}

	// A genuinely different scene passes.
	fresh := loom.NewTextResult("A bright morning sun rose over the distant hills as birds sang.", "stop", 1, 1)
	if _, err := hook(ctx, authorReq(scene), fresh); err != nil {
		t.Fatalf("fresh scene should pass: %v", err)
	}

	// No previous prose → nothing to compare → pass.
	if _, err := hook(ctx, authorReq(""), loom.NewTextResult(scene, "stop", 1, 1)); err != nil {
		t.Fatalf("first turn should pass: %v", err)
	}
}

func TestTrigramOverlap(t *testing.T) {
	s := "the quick brown fox jumps over the lazy dog"
	if got := trigramOverlap(s, s); got != 1.0 {
		t.Fatalf("identical overlap = %v, want 1.0", got)
	}
	if got := trigramOverlap(s, "completely unrelated words here now"); got != 0 {
		t.Fatalf("disjoint overlap = %v, want 0", got)
	}
}
