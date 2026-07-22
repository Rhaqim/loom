package engine

// clientasks_test.go — regressions for the asks in loom-asks.md (§1 response
// format List, §2 flow active-version resolution, §5 per-step usage). Each
// pins the new contract so a revert fails here rather than in a consumer.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- §1: ResponseFormats().List ---

func TestAsk1_ResponseFormatList(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask1", map[string]Generator{"g": okGen{}}, PollerConfig{})

	mk := func(owner, slug, category string, version int) {
		t.Helper()
		if err := e.ResponseFormats().Create(ctx, &ResponseFormatRecord{
			Slug: slug, Version: version, Owner: owner, Category: category,
			Schema: map[string]any{"type": "object"}, StrictMode: true,
		}); err != nil {
			t.Fatalf("create %s/%s: %v", owner, slug, err)
		}
	}
	mk("tenant-a", "logician", "analysis", 1)
	mk("tenant-a", "logician", "analysis", 2)
	mk("tenant-a", "titler", "meta", 1)
	mk("tenant-b", "other", "analysis", 1)

	got, err := e.ResponseFormats().List(ctx, "tenant-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("List(tenant-a) = %d records, want 3", len(got))
	}
	for _, r := range got {
		if r.Owner != "tenant-a" {
			t.Fatalf("List leaked a record owned by %q", r.Owner)
		}
		// The index deliberately omits schema bodies.
		if r.Schema != nil {
			t.Fatalf("List returned a schema body for %s (should be nil)", r.Slug)
		}
	}
	// Newest version first within a slug, so a picker can show the current one.
	if got[0].Slug != "logician" || got[0].Version != 2 {
		t.Fatalf("first record = %s@v%d, want logician@v2", got[0].Slug, got[0].Version)
	}

	// Category filter.
	filtered, err := e.ResponseFormats().List(ctx, "tenant-a", "meta")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Slug != "titler" {
		t.Fatalf("List(tenant-a, meta) = %v, want just titler", filtered)
	}

	// Category round-trips through a full read.
	full, err := e.ResponseFormats().Get(ctx, "tenant-a", "titler", 1)
	if err != nil {
		t.Fatal(err)
	}
	if full.Category != "meta" {
		t.Fatalf("Category = %q, want meta", full.Category)
	}
	if full.Schema == nil {
		t.Fatal("Get returned no schema body")
	}
}

// --- §2: flow active-version resolution ---

func mkFlow(t *testing.T, e *Engine, owner, slug string, version int, active bool) {
	t.Helper()
	if err := e.Flows().Create(context.Background(), &FlowRecord{
		Owner: owner, Slug: slug, Version: version, IsActive: active,
		Agents: []FlowAgentEntry{{AgentSlug: "lead", OutputKey: "Lead"}},
	}); err != nil {
		t.Fatalf("create flow %s v%d: %v", slug, version, err)
	}
}

// The core footgun: an inactive draft must not take the slug down.
func TestAsk2_DraftDoesNotShadowLiveVersion(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask2", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")

	mkFlow(t, e, "", "story", 4, true)  // live
	mkFlow(t, e, "", "story", 5, false) // half-finished draft

	// Version 0 is the serving path: it must resolve v4, not fail on v5.
	got, err := e.Flows().Get(ctx, "", "story", 0)
	if err != nil {
		t.Fatalf("Get(version 0) with a newer inactive draft: %v", err)
	}
	if got.Version != 4 {
		t.Fatalf("Get(version 0) = v%d, want v4 (the latest ACTIVE)", got.Version)
	}

	// Latest keeps the authoring meaning: highest version, drafts included.
	latest, err := e.Flows().Latest(ctx, "", "story")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 5 {
		t.Fatalf("Latest = v%d, want v5 (highest, including drafts)", latest.Version)
	}

	// And execution follows the serving path.
	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	turn, err := e.RunTurnBySlug(ctx, sess, "", "story", 0, TurnRequest{})
	if err != nil {
		t.Fatalf("RunTurnBySlug(version 0) with a newer draft: %v", err)
	}
	if turn.Lead == nil {
		t.Fatal("turn produced no lead step")
	}
}

func TestAsk2_LatestActiveAndSetActive(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask2b", map[string]Generator{"g": okGen{}}, PollerConfig{})

	mkFlow(t, e, "", "story", 1, true)
	mkFlow(t, e, "", "story", 2, true)
	mkFlow(t, e, "", "story", 3, true)

	// All active (the historical default) — highest wins.
	got, err := e.Flows().LatestActive(ctx, "", "story")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 3 {
		t.Fatalf("LatestActive = v%d, want v3", got.Version)
	}

	// Roll BACK to v1 — impossible before SetActive without writing a v4.
	if err := e.Flows().SetActive(ctx, "", "story", 1); err != nil {
		t.Fatalf("SetActive(v1): %v", err)
	}
	got, err = e.Flows().LatestActive(ctx, "", "story")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("after SetActive(v1), LatestActive = v%d, want v1", got.Version)
	}
	// Siblings deactivated, so exactly one version serves.
	for _, v := range []int{2, 3} {
		r, err := e.Flows().Get(ctx, "", "story", v)
		if err != nil {
			t.Fatal(err)
		}
		if r.IsActive {
			t.Fatalf("v%d is still active after SetActive(v1)", v)
		}
	}

	// A bad version must not leave the slug with nothing serving.
	if err := e.Flows().SetActive(ctx, "", "story", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetActive(v99) = %v, want ErrNotFound", err)
	}
	got, err = e.Flows().LatestActive(ctx, "", "story")
	if err != nil {
		t.Fatalf("a failed SetActive broke resolution: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("after a failed SetActive, LatestActive = v%d, want v1 unchanged", got.Version)
	}

	// version < 1 is rejected: 0 is reserved for latest-resolution.
	if err := e.Flows().SetActive(ctx, "", "story", 0); err == nil {
		t.Fatal("SetActive(v0) was accepted; want an error")
	}
}

// No active version at all is a clean not-found, not a nil deref.
func TestAsk2_NoActiveVersion(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask2c", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mkFlow(t, e, "", "draft-only", 1, false)

	if _, err := e.Flows().LatestActive(ctx, "", "draft-only"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestActive with no active version = %v, want ErrNotFound", err)
	}
	if _, err := e.Flows().Get(ctx, "", "draft-only", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(version 0) with no active version = %v, want ErrNotFound", err)
	}
	// Pinning the inactive version explicitly still refuses at execution.
	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunTurnBySlug(ctx, sess, "", "draft-only", 1, TurnRequest{}); err == nil {
		t.Fatal("RunTurnBySlug on a pinned inactive version succeeded; want an error")
	}
}

// --- §5: per-step usage ---

func TestAsk5_CostByStep(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask5", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	var stepIDs []string
	for range 3 {
		step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
		if err != nil {
			t.Fatal(err)
		}
		stepIDs = append(stepIDs, step.ID.String())
	}

	// Cost recording is fire-and-forget, so wait for it to settle.
	var usage []StepUsage
	for range 50 {
		var err error
		usage, err = e.Cost().ByStep(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(usage) == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(usage) != 3 {
		t.Fatalf("ByStep = %d rows, want 3 (one per step)", len(usage))
	}

	seen := map[string]bool{}
	for _, u := range usage {
		if u.StepID == uuid.Nil {
			t.Fatal("ByStep returned a row with no StepID")
		}
		seen[u.StepID.String()] = true
		if u.InputTokens == 0 && u.OutputTokens == 0 {
			t.Fatalf("step %s has no token counts", u.StepID)
		}
		// No pricing configured on this engine, so USD is a placeholder and
		// must say so.
		if !u.Estimated {
			t.Fatalf("step %s: Estimated = false with no pricing configured", u.StepID)
		}
	}
	for _, id := range stepIDs {
		if !seen[id] {
			t.Fatalf("ByStep omitted step %s", id)
		}
	}

	// The per-step rows must reconcile with the aggregate.
	total, err := e.Cost().SessionUsage(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tokens int
	for _, u := range usage {
		tokens += u.InputTokens + u.OutputTokens
	}
	if tokens != total.TotalTokens {
		t.Fatalf("ByStep tokens = %d, SessionUsage.TotalTokens = %d; they must agree",
			tokens, total.TotalTokens)
	}
}
