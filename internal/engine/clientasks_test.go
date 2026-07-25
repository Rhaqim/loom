package engine

// clientasks_test.go — regressions for the asks in loom-asks.md (§1 response
// format List, §2 flow active-version resolution, §5 per-step usage). Each
// pins the new contract so a revert fails here rather than in a consumer.

import (
	"context"
	"errors"
	"strings"
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

	// Cost commits with the step, so this is readable immediately — no polling.
	usage, err := e.Cost().ByStep(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
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

// --- §8: cost durability and shutdown ---

// The core of §8: cost must be durable the instant RunStep returns. It used to
// be written from a detached goroutine, so a redeploy between the return and
// the insert silently dropped it and every aggregate under-reported.
func TestAsk8_CostIsDurableWhenRunStepReturns(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask8", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
	if err != nil {
		t.Fatal(err)
	}

	// Read immediately — no sleep, no retry loop. If this needs waiting, the
	// write is not part of the step transaction.
	usage, err := e.Cost().ByStep(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 {
		t.Fatalf("ByStep immediately after RunStep = %d rows, want 1 "+
			"(cost is not committing with the step)", len(usage))
	}
	if usage[0].StepID != step.ID {
		t.Fatalf("cost row StepID = %s, want %s", usage[0].StepID, step.ID)
	}
}

// The savepoint contract: the provider has already been billed by the time the
// cost row is written, so a cost failure must lose the bookkeeping row, never
// the paid-for step.
func TestAsk8_CostFailureDoesNotDiscardStep(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "ask8fail", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// Break only the cost write. Everything else in the step transaction is
	// untouched, which is exactly the independent-failure case the SAVEPOINT
	// exists for.
	if _, err := db.ExecContext(ctx, `DROP TABLE loom_cost_records`); err != nil {
		t.Fatal(err)
	}

	step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
	if err != nil {
		t.Fatalf("a cost-write failure discarded the step: %v", err)
	}
	if step == nil {
		t.Fatal("step is nil after a contained cost failure")
	}

	// The step, its result and its checkpoint must all be durable.
	got, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 {
		t.Fatalf("history = %d steps, want 1 — the step was rolled back", len(got.History))
	}
	if _, found, err := e.Sessions().StateAt(ctx, sess.ID, step.Index); err != nil || !found {
		t.Fatalf("checkpoint missing after contained cost failure: found=%v err=%v", found, err)
	}
}

// Close must be usable on any engine, including one whose poller never ran,
// and must be safe to call twice (a shutdown path may be reached by more than
// one route).
func TestAsk8_CloseIsSafeAndIdempotent(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask8close", map[string]Generator{"g": okGen{}}, PollerConfig{})

	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close on an engine with no poller: %v", err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Close stops the poller and waits for it, so that once it returns nothing is
// still touching the database — the condition that makes teardown safe.
func TestAsk8_CloseDrainsPoller(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask8drain", map[string]Generator{"g": okGen{}},
		PollerConfig{Workers: 2, Interval: time.Millisecond})

	e.StartPoller(ctx)
	// Let it tick a few times so there is real in-flight work to drain.
	time.Sleep(20 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- e.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return — the poller drain is not terminating")
	}

	// Starting again after Close must work rather than silently no-op, so a
	// restart path is available.
	e.StartPoller(ctx)
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close after restart: %v", err)
	}
}

// A caller that cancels the poller's own context still gets a clean Close.
func TestAsk8_CloseAfterContextCancel(t *testing.T) {
	pollCtx, cancel := context.WithCancel(context.Background())
	e, _ := reproEngine(t, "ask8cancel", map[string]Generator{"g": okGen{}},
		PollerConfig{Workers: 1, Interval: time.Millisecond})

	e.StartPoller(pollCtx)
	time.Sleep(10 * time.Millisecond)
	cancel() // stop via the caller's context, not Close

	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close after the poller context was cancelled: %v", err)
	}
}

// --- §9 / §10 / §11: lineage and retention ---

// buildChain returns root..head, each session forked from the previous and
// carrying `steps` steps, mirroring a one-session-per-turn playthrough.
func buildChain(t *testing.T, e *Engine, turns, steps int) []*Session {
	t.Helper()
	ctx := context.Background()
	root := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	for range steps {
		if _, err := e.RunStep(ctx, root, StepRequest{AgentSlug: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	chain := []*Session{root}
	cur := root
	for range turns {
		b, err := e.Sessions().Fork(ctx, cur.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for range steps {
			if _, err := e.RunStep(ctx, b, StepRequest{AgentSlug: "a"}); err != nil {
				t.Fatal(err)
			}
		}
		chain = append(chain, b)
		cur = b
	}
	return chain
}

func TestAsk9_BranchTreeCarriesStepCount(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask9", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	chain := buildChain(t, e, 2, 3) // root + 2 forks, 3 steps each
	// A second branch off the root, so the tree is not a straight line.
	sib, err := e.Sessions().Fork(ctx, chain[0].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sib, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}

	tree, err := e.Sessions().BranchTree(ctx, chain[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if tree.StepCount != 3 {
		t.Fatalf("root StepCount = %d, want 3", tree.StepCount)
	}
	// History is deliberately not loaded, so len(History) is NOT a step count —
	// that asymmetry is exactly why StepCount exists.
	if len(tree.Session.History) != 0 {
		t.Fatalf("BranchTree loaded History (%d steps); it must stay header-only",
			len(tree.Session.History))
	}
	if len(tree.Children) != 2 {
		t.Fatalf("root children = %d, want 2", len(tree.Children))
	}

	// Walk the whole tree; every node must carry its own count.
	var walk func(*BranchNode) int
	walk = func(n *BranchNode) int {
		total := n.StepCount
		for _, c := range n.Children {
			total += walk(c)
		}
		return total
	}
	// 3 (root) + 3 + 3 (chain) + 1 (sibling) = 10
	if got := walk(tree); got != 10 {
		t.Fatalf("total StepCount across tree = %d, want 10", got)
	}
}

func TestAsk10_Ancestry(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask10", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	chain := buildChain(t, e, 4, 1)
	head := chain[len(chain)-1]

	// An abandoned rewind branch must not appear in the head's lineage, even
	// though BranchTree would return it.
	if _, err := e.Sessions().Fork(ctx, chain[1].ID, 0); err != nil {
		t.Fatal(err)
	}

	line, err := e.Sessions().Ancestry(ctx, head.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) != len(chain) {
		t.Fatalf("Ancestry returned %d sessions, want %d", len(line), len(chain))
	}
	for i, s := range line {
		if s.ID != chain[i].ID {
			t.Fatalf("Ancestry[%d] = %s, want %s (order must be root-first)", i, s.ID, chain[i].ID)
		}
		if s.History != nil {
			t.Fatalf("Ancestry[%d] loaded History; it must be header-only", i)
		}
	}

	// A root resolves to just itself.
	rootLine, err := e.Sessions().Ancestry(ctx, chain[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootLine) != 1 || rootLine[0].ID != chain[0].ID {
		t.Fatalf("Ancestry(root) = %d sessions, want exactly the root", len(rootLine))
	}

	if _, err := e.Sessions().Ancestry(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Ancestry of a missing session = %v, want ErrNotFound", err)
	}
}

// §11: an unpinned ancestor is protected purely by having a live descendant, so
// a chain only needs its HEAD pinned. This is load-bearing guidance — if the
// child-exists guard is ever dropped from a GC tier, callers who pinned only
// the head would silently lose history, so it is pinned by a test.
func TestAsk11_HeadPinProtectsWholeChain(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "ask11", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	chain := buildChain(t, e, 3, 1)
	head := chain[len(chain)-1]
	if err := e.Sessions().Pin(ctx, head.ID); err != nil {
		t.Fatal(err)
	}

	// Age every session far past every tier's retention.
	old := time.Now().Add(-365 * 24 * time.Hour)
	for _, s := range chain {
		if _, err := db.ExecContext(ctx,
			`UPDATE loom_sessions SET created_at=$1, updated_at=$2 WHERE id=$3`, old, old, s.ID); err != nil {
			t.Fatal(err)
		}
	}

	// Sweep repeatedly: an unravelling chain would lose one link per pass.
	for range 5 {
		if _, err := e.GC().Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}

	for i, s := range chain {
		if _, err := e.Sessions().GetHeader(ctx, s.ID); err != nil {
			t.Fatalf("chain[%d] (%s) was collected despite a live descendant: %v", i, s.ID, err)
		}
	}
	// And the lineage still reads end to end.
	line, err := e.Sessions().Ancestry(ctx, head.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) != len(chain) {
		t.Fatalf("Ancestry after GC = %d sessions, want %d", len(line), len(chain))
	}
}

// §11: Pin + Discard is the archive tier — readable, invisible to List, and
// never hard-deleted, since every GC tier requires pinned = false.
func TestAsk11_PinnedDiscardedIsArchived(t *testing.T) {
	ctx := context.Background()
	e, db := reproEngine(t, "ask11b", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Sessions().Pin(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Sessions().Discard(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-365 * 24 * time.Hour)
	if _, err := db.ExecContext(ctx,
		`UPDATE loom_sessions SET created_at=$1, updated_at=$2, deleted_at=$3 WHERE id=$4`,
		old, old, old, sess.ID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := e.GC().Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Still readable with its history...
	got, err := e.Sessions().GetIncludingDeleted(ctx, sess.ID)
	if err != nil {
		t.Fatalf("archived session was hard-deleted: %v", err)
	}
	if len(got.History) != 1 {
		t.Fatalf("archived history = %d steps, want 1", len(got.History))
	}
	// ...but no longer live.
	if _, err := e.Sessions().Get(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archived session is still live: %v", err)
	}
	list, err := e.Sessions().List(ctx, "p", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == sess.ID {
			t.Fatal("archived session still appears in List")
		}
	}
}

// --- §12: step index must come from persisted state, not in-memory history ---

// GetHeader exists so a caller can resume without loading the transcript. When
// the index was len(session.History) that was impossible: a header-loaded
// session has History == nil, so every step restarted at 0 and collided with
// the rows already there.
func TestAsk12_HeaderLoadedSessionContinuesStepIndex(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask12", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Five turns, each resuming the cheap way — no transcript ever loaded.
	for turn := range 5 {
		h, err := e.Sessions().GetHeader(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if h.History != nil {
			t.Fatal("GetHeader loaded History; the premise of this test is gone")
		}
		step, err := e.RunStep(ctx, h, StepRequest{AgentSlug: "a"})
		if err != nil {
			t.Fatalf("turn %d on a header-loaded session: %v", turn, err)
		}
		if step.Index != turn {
			t.Fatalf("turn %d step index = %d, want %d", turn, step.Index, turn)
		}
	}

	steps, err := e.Sessions().Steps(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 5 {
		t.Fatalf("persisted steps = %d, want 5", len(steps))
	}
	for i, s := range steps {
		if s.Index != i {
			t.Fatalf("steps[%d].Index = %d, want %d — indices are not contiguous", i, s.Index, i)
		}
	}

	// Every step must keep its own checkpoint: a collision would have meant
	// rewind landing on the wrong state.
	for i := range 5 {
		st, found, err := e.Sessions().StateAt(ctx, sess.ID, i)
		if err != nil || !found {
			t.Fatalf("no checkpoint at index %d: found=%v err=%v", i, found, err)
		}
		_ = st
	}
}

// Two callers holding SEPARATE copies of the same session used to compute the
// same index from their own histories. The index now comes from the database,
// so the second one continues rather than colliding.
func TestAsk12_SeparateSessionCopiesDoNotCollide(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask12b", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}

	// Two independent loads, each with its own History slice.
	a, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sa, err := e.RunStep(ctx, a, StepRequest{AgentSlug: "a"})
	if err != nil {
		t.Fatal(err)
	}
	// Copy b is now a stale writer, so its SESSION-row write loses the version
	// CAS and returns ErrSessionNotPersisted. That is the documented
	// optimistic-concurrency contract and is separate from indexing: the step
	// itself is still committed, and what matters here is that its index
	// continued from the database instead of colliding with copy a's.
	sb, err := e.RunStep(ctx, b, StepRequest{AgentSlug: "a"})
	if err != nil && !errors.Is(err, ErrSessionNotPersisted) {
		t.Fatalf("unexpected error from the second copy: %v", err)
	}
	if sb == nil {
		t.Fatal("second copy produced no step")
	}
	if sa.Index == sb.Index {
		t.Fatalf("both copies produced index %d; the index is still derived from in-memory history", sa.Index)
	}
	if sa.Index != 1 || sb.Index != 2 {
		t.Fatalf("indices = %d,%d; want 1,2", sa.Index, sb.Index)
	}
	// Both steps are durable and contiguous.
	steps, err := e.Sessions().Steps(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("persisted steps = %d, want 3", len(steps))
	}
}

// A forked branch restarts at 0 — its indices are per-session, and the DB-derived
// index must respect that rather than continuing the parent's numbering.
func TestAsk12_ForkedBranchRestartsAtZero(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask12c", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	branch, err := e.Sessions().Fork(ctx, sess.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	step, err := e.RunStep(ctx, branch, StepRequest{AgentSlug: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if step.Index != 0 {
		t.Fatalf("first step on a fresh branch = index %d, want 0", step.Index)
	}
}

// Turn steps must still be numbered consecutively when a flow runs lead +
// followers against one session.
func TestAsk12_TurnStepsAreConsecutive(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "ask12d", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	mustAgent(t, e, "f1", "g")

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	flow := Flow{
		Slug:      "t",
		Lead:      FlowAgent{AgentSlug: "lead", OutputKey: "Lead"},
		Followers: []FlowAgent{{AgentSlug: "f1", OutputKey: "F1"}},
	}
	// Two turns, each resuming from a header — the session-per-story shape.
	for turn := range 2 {
		h, err := e.Sessions().GetHeader(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		tr, err := e.RunTurn(ctx, h, TurnRequest{Flow: flow})
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if len(tr.Steps) != 2 {
			t.Fatalf("turn %d produced %d steps, want 2", turn, len(tr.Steps))
		}
	}
	steps, err := e.Sessions().Steps(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 4 {
		t.Fatalf("persisted steps = %d, want 4 (2 turns x 2 agents)", len(steps))
	}
	for i, s := range steps {
		if s.Index != i {
			t.Fatalf("steps[%d].Index = %d, want %d", i, s.Index, i)
		}
	}
}

// --- §2.2 (declaration-only): Prompt.Variables enforced at render ---

func TestVars_DeclaredVariableMustBeSupplied(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "vars", map[string]Generator{"g": okGen{}}, PollerConfig{})

	// A user template that requires two inputs and declares them.
	sys := &Prompt{Slug: "sys", Version: 1, Kind: PromptKindSystem, Body: "sys"}
	if err := e.Prompts().Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	ut := &Prompt{
		Slug: "ut", Version: 1, Kind: PromptKindUserTemplate,
		Body:      "Hello {{.Inputs.name}}, you are in {{.Inputs.room}}.",
		Variables: []string{"name", "room"},
	}
	if err := e.Prompts().Create(ctx, ut); err != nil {
		t.Fatal(err)
	}
	if err := e.Agents().Create(ctx, &Agent{
		Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g",
		SystemPromptID: sys.ID, UserTemplateID: ut.ID,
	}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Missing "room" — must fail with ErrMissingVariables naming it, NOT render
	// an empty room.
	_, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a", Inputs: map[string]any{"name": "Ada"}})
	if !errors.Is(err, ErrMissingVariables) {
		t.Fatalf("missing variable: err = %v, want ErrMissingVariables", err)
	}
	if err == nil || !contains(err.Error(), "room") {
		t.Fatalf("error %v should name the missing variable 'room'", err)
	}

	// All supplied — succeeds.
	if _, err := e.RunStep(ctx, sess, StepRequest{
		AgentSlug: "a", Inputs: map[string]any{"name": "Ada", "room": "cellar"},
	}); err != nil {
		t.Fatalf("all variables supplied: %v", err)
	}
}

// A key present with an empty or nil value counts as supplied — the caller made
// a deliberate choice; only an ABSENT key is the forgotten/misspelled case.
func TestVars_PresentButEmptyIsSupplied(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "vars_empty", map[string]Generator{"g": okGen{}}, PollerConfig{})
	sys := &Prompt{Slug: "s", Version: 1, Kind: PromptKindSystem, Body: "s"}
	_ = e.Prompts().Create(ctx, sys)
	ut := &Prompt{Slug: "u", Version: 1, Kind: PromptKindUserTemplate,
		Body: "[{{.Inputs.note}}]", Variables: []string{"note"}}
	_ = e.Prompts().Create(ctx, ut)
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText,
		GeneratorSlug: "g", SystemPromptID: sys.ID, UserTemplateID: ut.ID}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p"}
	_ = e.Sessions().Create(ctx, sess)

	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a", Inputs: map[string]any{"note": ""}}); err != nil {
		t.Fatalf("empty-string value should satisfy the declaration: %v", err)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a", Inputs: map[string]any{"note": nil}}); err != nil {
		t.Fatalf("nil value (key present) should satisfy the declaration: %v", err)
	}
}

// A prompt that declares nothing behaves exactly as before — no enforcement.
func TestVars_UndeclaredIsBackwardCompatible(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "vars_compat", map[string]Generator{"g": okGen{}}, PollerConfig{})
	sys := &Prompt{Slug: "s", Version: 1, Kind: PromptKindSystem, Body: "s"}
	_ = e.Prompts().Create(ctx, sys)
	// References an input but declares no Variables — renders empty, no error,
	// as it always did.
	ut := &Prompt{Slug: "u", Version: 1, Kind: PromptKindUserTemplate,
		Body: "Hi {{.Inputs.name}}"}
	_ = e.Prompts().Create(ctx, ut)
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText,
		GeneratorSlug: "g", SystemPromptID: sys.ID, UserTemplateID: ut.ID}); err != nil {
		t.Fatal(err)
	}
	sess := &Session{PlatformID: "p"}
	_ = e.Sessions().Create(ctx, sess)
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatalf("undeclared template must not enforce: %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// --- §2.2 (producer/consumer, phase 1): Flow.Inputs + Flows().Validate ---

// helper: create an agent whose user template declares the given required vars.
func agentWithVars(t *testing.T, e *Engine, slug string, vars []string) {
	t.Helper()
	ctx := context.Background()
	ut := &Prompt{Slug: slug + "-ut", Version: 1, Kind: PromptKindUserTemplate,
		Body: "x", Variables: vars}
	if err := e.Prompts().Create(ctx, ut); err != nil {
		t.Fatal(err)
	}
	if err := e.Agents().Create(ctx, &Agent{
		Slug: slug, Version: 1, Modal: ModalityText, GeneratorSlug: "g",
		UserTemplateID: ut.ID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFlowValidate_InputsRoundTrip(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "fv_rt", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Inputs: []string{"topic", "tone"},
		Agents: []FlowAgentEntry{{AgentSlug: "lead", OutputKey: "Lead"}},
	}
	if err := e.Flows().Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := e.Flows().Get(ctx, "", "f", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Inputs) != 2 || got.Inputs[0] != "topic" || got.Inputs[1] != "tone" {
		t.Fatalf("Inputs round-trip = %v, want [topic tone]", got.Inputs)
	}
}

func TestFlowValidate_ConsumedFromLeadAndInputs(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "fv_ok", map[string]Generator{"g": okGen{}}, PollerConfig{})
	// lead consumes an external input; follower consumes the lead's output.
	agentWithVars(t, e, "lead", []string{"topic"})
	agentWithVars(t, e, "f1", []string{"Lead"})

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Inputs: []string{"topic"},
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Lead"},
			{AgentSlug: "f1", OutputKey: "F1"},
		},
	}
	if err := e.Flows().Validate(ctx, rec); err != nil {
		t.Fatalf("valid wiring rejected: %v", err)
	}
}

func TestFlowValidate_DanglingConsumedVariable(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "fv_dangle", map[string]Generator{"g": okGen{}}, PollerConfig{})
	// Consumes "topic" but nobody produces it and it is not declared as an input.
	agentWithVars(t, e, "lead", []string{"topic"})

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Agents: []FlowAgentEntry{{AgentSlug: "lead", OutputKey: "Lead"}},
	}
	err := e.Flows().Validate(ctx, rec)
	if !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("err = %v, want ErrFlowInvalid", err)
	}
	if err == nil || !strings.Contains(err.Error(), "topic") {
		t.Fatalf("error %v should name the dangling variable 'topic'", err)
	}
	// Declaring it as an input fixes it.
	rec.Inputs = []string{"topic"}
	if err := e.Flows().Validate(ctx, rec); err != nil {
		t.Fatalf("after declaring input: %v", err)
	}
}

// Phase 2: an ACYCLIC cross-follower dependency is now valid (f2 consumes f1).
func TestFlowValidate_CrossFollowerAcyclicIsValid(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "fv_cross", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	agentWithVars(t, e, "f1", nil)
	agentWithVars(t, e, "f2", []string{"F1"})

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Lead"},
			{AgentSlug: "f1", OutputKey: "F1"},
			{AgentSlug: "f2", OutputKey: "F2"},
		},
	}
	if err := e.Flows().Validate(ctx, rec); err != nil {
		t.Fatalf("acyclic cross-follower wiring rejected: %v", err)
	}
}

// Phase 2: a dependency CYCLE among followers is rejected.
func TestFlowValidate_CycleRejected(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "fv_cycle", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	agentWithVars(t, e, "f1", []string{"F2"}) // f1 consumes f2
	agentWithVars(t, e, "f2", []string{"F1"}) // f2 consumes f1

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Lead"},
			{AgentSlug: "f1", OutputKey: "F1"},
			{AgentSlug: "f2", OutputKey: "F2"},
		},
	}
	err := e.Flows().Validate(ctx, rec)
	if !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("err = %v, want ErrFlowInvalid", err)
	}
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error %v should name the dependency cycle", err)
	}
}

// The lead runs first, so it cannot consume a follower's output.
func TestFlowValidate_LeadCannotConsumeFollower(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "fv_lead", map[string]Generator{"g": okGen{}}, PollerConfig{})
	agentWithVars(t, e, "lead", []string{"F1"}) // lead consumes a follower's output
	agentWithVars(t, e, "f1", nil)

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Lead"},
			{AgentSlug: "f1", OutputKey: "F1"},
		},
	}
	err := e.Flows().Validate(ctx, rec)
	if !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("err = %v, want ErrFlowInvalid", err)
	}
	if err == nil || !strings.Contains(err.Error(), "lead") {
		t.Fatalf("error %v should explain the lead cannot consume a follower", err)
	}
}

// Duplicate producer names are caught by the self-contained check, in BOTH
// Validate and Create (no agent resolution needed).
func TestFlowValidate_DuplicateProducerRejectedAtCreate(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "fv_dup", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	mustAgent(t, e, "f1", "g")

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Shared"},
			{AgentSlug: "f1", OutputKey: "Shared"},
		},
	}
	err := e.Flows().Create(ctx, rec)
	if !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("Create with duplicate output key: err = %v, want ErrFlowInvalid", err)
	}
	if err == nil || !strings.Contains(err.Error(), "Shared") {
		t.Fatalf("error %v should name the duplicated output key", err)
	}
}

// A flow that declares no variables and has unique keys is valid — full
// backward compatibility for existing flows.
func TestFlowValidate_UndeclaredIsValid(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "fv_compat", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	mustAgent(t, e, "f1", "g")

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Lead"},
			{AgentSlug: "f1", OutputKey: "F1"},
		},
	}
	if err := e.Flows().Validate(ctx, rec); err != nil {
		t.Fatalf("a flow that declares no wiring must be valid: %v", err)
	}
	if err := e.Flows().Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// --- Flows().Plan: resolved wiring for visualization ---

// planNode / planEdge lookup helpers keep the assertions readable.
func planNode(p *FlowPlan, slug string) *FlowPlanNode {
	for i := range p.Nodes {
		if p.Nodes[i].AgentSlug == slug {
			return &p.Nodes[i]
		}
	}
	return nil
}
func planEdge(p *FlowPlan, to, varName string) *FlowPlanEdge {
	for i := range p.Edges {
		if p.Edges[i].To == to && p.Edges[i].Var == varName {
			return &p.Edges[i]
		}
	}
	return nil
}

func TestFlowPlan_NodesEdgesLayers(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "plan_ok", map[string]Generator{"g": okGen{}}, PollerConfig{})
	// lead consumes external "topic"; f1 consumes the lead; f2 consumes f1.
	agentWithVars(t, e, "lead", []string{"topic"})
	agentWithVars(t, e, "f1", []string{"Lead"})
	agentWithVars(t, e, "f2", []string{"F1"})

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Inputs: []string{"topic"},
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Lead"},
			{AgentSlug: "f1", OutputKey: "F1"},
			{AgentSlug: "f2", OutputKey: "F2"},
		},
	}
	plan, err := e.Flows().Plan(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}

	// Layers: lead=0, f1=1 (consumes lead), f2=2 (consumes f1).
	if plan.Layers != 2 {
		t.Fatalf("Layers = %d, want 2", plan.Layers)
	}
	if n := planNode(plan, "lead"); n == nil || !n.IsLead || n.Layer != 0 || n.OutputKey != "Lead" {
		t.Fatalf("lead node wrong: %+v", n)
	}
	if n := planNode(plan, "f1"); n == nil || n.Layer != 1 {
		t.Fatalf("f1 layer = %v, want 1", n)
	}
	if n := planNode(plan, "f2"); n == nil || n.Layer != 2 {
		t.Fatalf("f2 layer = %v, want 2", n)
	}

	// Edges: topic is an external input; Lead→f1 and F1→f2 are agent edges.
	if ed := planEdge(plan, "lead", "topic"); ed == nil || ed.Source != FlowEdgeInput {
		t.Fatalf("lead<-topic edge = %+v, want source input", ed)
	}
	if ed := planEdge(plan, "f1", "Lead"); ed == nil || ed.Source != FlowEdgeAgent || ed.From != "lead" {
		t.Fatalf("f1<-Lead edge = %+v, want agent edge from lead", ed)
	}
	if ed := planEdge(plan, "f2", "F1"); ed == nil || ed.Source != FlowEdgeAgent || ed.From != "f1" {
		t.Fatalf("f2<-F1 edge = %+v, want agent edge from f1", ed)
	}
	if len(plan.Cyclic) != 0 {
		t.Fatalf("Cyclic = %v, want none", plan.Cyclic)
	}
	if len(plan.Inputs) != 1 || plan.Inputs[0] != "topic" {
		t.Fatalf("Inputs = %v, want [topic]", plan.Inputs)
	}
}

// Plan represents a broken flow rather than erroring: a dangling consumed
// variable is an unresolved edge, and a cycle fills Cyclic.
func TestFlowPlan_RepresentsProblems(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "plan_bad", map[string]Generator{"g": okGen{}}, PollerConfig{})
	// f1 consumes "ghost" (nobody produces it); f2<->f3 form a cycle.
	agentWithVars(t, e, "lead", nil)
	agentWithVars(t, e, "f1", []string{"ghost"})
	agentWithVars(t, e, "f2", []string{"F3"})
	agentWithVars(t, e, "f3", []string{"F2"})

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Lead"},
			{AgentSlug: "f1", OutputKey: "F1"},
			{AgentSlug: "f2", OutputKey: "F2"},
			{AgentSlug: "f3", OutputKey: "F3"},
		},
	}
	// Plan does NOT error on a broken flow.
	plan, err := e.Flows().Plan(ctx, rec)
	if err != nil {
		t.Fatalf("Plan should represent a broken flow, not error: %v", err)
	}
	if ed := planEdge(plan, "f1", "ghost"); ed == nil || ed.Source != FlowEdgeUnresolved {
		t.Fatalf("f1<-ghost edge = %+v, want source unresolved", ed)
	}
	// f2 and f3 are cyclic.
	if len(plan.Cyclic) != 2 {
		t.Fatalf("Cyclic = %v, want f2 and f3", plan.Cyclic)
	}

	// And the plan's verdict matches Validate: this flow is invalid.
	if err := e.Flows().Validate(ctx, rec); !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("Validate = %v, want ErrFlowInvalid (must agree with Plan's unresolved/cyclic)", err)
	}
}

// A plan is "valid" exactly when it has no unresolved edge and no cycle — Plan
// and Validate must agree.
func TestFlowPlan_AgreesWithValidate(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "plan_agree", map[string]Generator{"g": okGen{}}, PollerConfig{})
	agentWithVars(t, e, "lead", []string{"topic"})
	agentWithVars(t, e, "f1", []string{"Lead"})

	rec := &FlowRecord{
		Slug: "f", Version: 1, IsActive: true,
		Inputs: []string{"topic"},
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Lead"},
			{AgentSlug: "f1", OutputKey: "F1"},
		},
	}
	plan, err := e.Flows().Plan(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	unresolved := 0
	for _, ed := range plan.Edges {
		if ed.Source == FlowEdgeUnresolved {
			unresolved++
		}
	}
	planValid := unresolved == 0 && len(plan.Cyclic) == 0
	validateOK := e.Flows().Validate(ctx, rec) == nil
	if planValid != validateOK {
		t.Fatalf("Plan valid=%v but Validate ok=%v — they must agree", planValid, validateOK)
	}
	if !planValid {
		t.Fatal("expected this flow to be valid")
	}
}
