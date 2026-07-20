package engine

// clientreport_test.go — regressions for the issues raised in loom-issues.md.
// Each test pins the FIXED contract, so a revert fails here rather than in a
// consumer. Named by the report's ids (B5..B9) for traceability.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// --- B6: Get must not resurrect a discarded session ---

func TestB6_DiscardedSessionIsNotReadable(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "b6", map[string]Generator{"g": okGen{}}, PollerConfig{})
	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := e.Sessions().Discard(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	// Get, GetHeader and Fork all route through the live-only read, so a
	// discarded session is invisible to every one of them.
	if _, err := e.Sessions().Get(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on discarded session: err = %v, want ErrNotFound", err)
	}
	if _, err := e.Sessions().GetHeader(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetHeader on discarded session: err = %v, want ErrNotFound", err)
	}
	// Forking a deleted parent would rewind its state into a fresh live session
	// — a resurrection path around Discard.
	if _, err := e.Sessions().Fork(ctx, sess.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Fork of discarded session: err = %v, want ErrNotFound", err)
	}

	// The explicit restore/audit path still sees it.
	got, err := e.Sessions().GetIncludingDeleted(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetIncludingDeleted: %v", err)
	}
	if got.DeletedAt == nil {
		t.Fatal("GetIncludingDeleted returned a session with nil DeletedAt")
	}
}

// A write against a missing row previously reported success. It must not.
func TestB6_WritesToMissingSessionAreNotSilent(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "b6b", map[string]Generator{"g": okGen{}}, PollerConfig{})
	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := e.Sessions().Discard(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	// Already discarded — a second Discard changes no rows and must say so
	// rather than returning nil.
	if err := e.Sessions().Discard(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-Discard: err = %v, want ErrNotFound", err)
	}
	if err := e.Sessions().Pin(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Pin on discarded session: err = %v, want ErrNotFound", err)
	}
}

// --- B8: Purge must respect a pin ---

func TestB8_PurgeRefusesPinnedSession(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "b8", map[string]Generator{"g": okGen{}}, PollerConfig{})
	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := e.Sessions().Pin(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	if err := e.Sessions().Purge(ctx, sess.ID); !errors.Is(err, ErrSessionPinned) {
		t.Fatalf("Purge of pinned session: err = %v, want ErrSessionPinned", err)
	}
	// Refusing must be non-destructive.
	if _, err := e.Sessions().Get(ctx, sess.ID); err != nil {
		t.Fatalf("session gone after a refused Purge: %v", err)
	}

	// ForcePurge is the documented override.
	if err := e.Sessions().ForcePurge(ctx, sess.ID); err != nil {
		t.Fatalf("ForcePurge: %v", err)
	}
	if _, err := e.Sessions().Get(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session still present after ForcePurge: err = %v", err)
	}
}

// --- B5: a hook writing State.Snapshot must not have it silently dropped ---

func TestB5_HookSnapshotWriteIsMerged(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "b5", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	want := []byte(`{"room":"cellar"}`)
	e.Hooks().RegisterPre("b5", func(_ context.Context, req *StepRequest) error {
		req.Session.State.Snapshot = want
		req.Session.State.Vars = map[string]any{"hp": 3}
		return nil
	})

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	step, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
	if err != nil {
		t.Fatal(err)
	}

	if string(sess.State.Snapshot) != string(want) {
		t.Fatalf("in-memory Snapshot = %q, want %q", sess.State.Snapshot, want)
	}
	if sess.State.Vars["hp"] != 3 {
		t.Fatalf("in-memory Vars[hp] = %v, want 3", sess.State.Vars["hp"])
	}
	// It must reach the checkpoint too, not just memory — the checkpoint is what
	// Fork and StateAt read back.
	st, found, err := e.Sessions().StateAt(ctx, sess.ID, step.Index)
	if err != nil || !found {
		t.Fatalf("StateAt: found %v, err %v", found, err)
	}
	if string(st.Snapshot) != string(want) {
		t.Fatalf("checkpointed Snapshot = %q, want %q", st.Snapshot, want)
	}
}

// A hook that never touches Snapshot must not clear an existing one.
func TestB5_UntouchedSnapshotSurvives(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "b5b", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	sess := &Session{PlatformID: "p", State: State{Snapshot: []byte(`{"keep":true}`)}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
		t.Fatal(err)
	}
	if string(sess.State.Snapshot) != `{"keep":true}` {
		t.Fatalf("Snapshot = %q, want it preserved", sess.State.Snapshot)
	}
}

// --- B7: a persisted flow must keep its retry policy ---

func TestB7_FlowRetryPolicyRoundTrips(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "b7", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "lead", "g")
	mustAgent(t, e, "f1", "g")

	rec := &FlowRecord{
		Slug: "story", Version: 1, IsActive: true,
		Agents: []FlowAgentEntry{
			{AgentSlug: "lead", OutputKey: "Lead", RetryMode: RetryKeepBest, MaxRetries: 5},
			{AgentSlug: "f1", OutputKey: "F1", RetryMode: RetryKeepBest, MaxRetries: 2},
		},
	}
	if err := e.Flows().Create(ctx, rec); err != nil {
		t.Fatal(err)
	}

	got, err := e.Flows().Get(ctx, "", "story", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agents[0].RetryMode != RetryKeepBest || got.Agents[0].MaxRetries != 5 {
		t.Fatalf("lead entry: RetryMode=%v MaxRetries=%d, want RetryKeepBest/5",
			got.Agents[0].RetryMode, got.Agents[0].MaxRetries)
	}
	// And it must survive the conversion to the runtime Flow, which is what
	// actually drives execution.
	f := got.Flow()
	if f.Lead.RetryMode != RetryKeepBest || f.Lead.MaxRetries != 5 {
		t.Fatalf("runtime lead: RetryMode=%v MaxRetries=%d, want RetryKeepBest/5",
			f.Lead.RetryMode, f.Lead.MaxRetries)
	}
	if f.Followers[0].RetryMode != RetryKeepBest || f.Followers[0].MaxRetries != 2 {
		t.Fatalf("runtime follower: RetryMode=%v MaxRetries=%d, want RetryKeepBest/2",
			f.Followers[0].RetryMode, f.Followers[0].MaxRetries)
	}
}

// --- B9: USD must be marked synthetic when no pricing is configured ---

func TestB9_PricingConfiguredFlag(t *testing.T) {
	ctx := context.Background()

	// No Config.Pricing and no DefaultPrice — any USD is a placeholder.
	e, _ := reproEngine(t, "b9", map[string]Generator{"g": okGen{}}, PollerConfig{})
	u, err := e.Cost().Usage(ctx, UsageQuery{Target: BudgetTarget{Kind: TargetPlatformID, Key: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if u.PricingConfigured {
		t.Fatal("PricingConfigured = true with no pricing configured")
	}

	// With a default price it is real money.
	db := e.db
	priced, err := New(Config{
		DB: db, Dialect: DialectSQLite,
		Generators:   map[string]Generator{"g": okGen{}},
		DefaultPrice: &ModelPrice{InputPer1M: 2, OutputPer1M: 6},
	})
	if err != nil {
		t.Fatal(err)
	}
	u2, err := priced.Cost().Usage(ctx, UsageQuery{Target: BudgetTarget{Kind: TargetPlatformID, Key: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if !u2.PricingConfigured {
		t.Fatal("PricingConfigured = false despite Config.DefaultPrice")
	}
}

// --- G3: header-only and paged reads must not load full history ---

func TestG3_HeaderAndPagedReads(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "g3", map[string]Generator{"g": okGen{}}, PollerConfig{})
	mustAgent(t, e, "a", "g")

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"}); err != nil {
			t.Fatal(err)
		}
	}

	full, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.History) != 5 {
		t.Fatalf("Get History = %d steps, want 5", len(full.History))
	}

	// History nil (not empty) so a caller can tell "not loaded" from "no steps".
	head, err := e.Sessions().GetHeader(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if head.History != nil {
		t.Fatalf("GetHeader History = %v, want nil", head.History)
	}
	if head.Version != full.Version {
		t.Fatalf("GetHeader Version = %d, want %d", head.Version, full.Version)
	}

	page, err := e.Sessions().Steps(ctx, sess.ID, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("Steps(limit 2, offset 1) = %d steps, want 2", len(page))
	}
	if page[0].Index != 1 || page[1].Index != 2 {
		t.Fatalf("Steps page indices = %d,%d; want 1,2", page[0].Index, page[1].Index)
	}

	// limit <= 0 means "no limit".
	all, err := e.Sessions().Steps(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("Steps(no limit) = %d steps, want 5", len(all))
	}
}

// --- G4: owner must isolate tenants at EXECUTION time, not just on reads ---

// Registry scoping alone does not make step execution tenant-safe: RunStep
// resolves an agent by slug, so if the owner is not threaded onto StepRequest a
// tenant can execute another tenant's agent.
func TestG4_StepExecutionIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "g4", map[string]Generator{"g": okGen{}}, PollerConfig{})

	// Same slug, two owners — only possible because the unique key includes owner.
	for _, owner := range []string{"tenant-a", "tenant-b"} {
		if err := e.Agents().Create(ctx, &Agent{
			Slug: "narrator", Version: 1, Owner: owner,
			Modal: ModalityText, GeneratorSlug: "g",
		}); err != nil {
			t.Fatalf("create agent for %s: %v", owner, err)
		}
	}

	sess := &Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// tenant-a runs its own agent: fine.
	if _, err := e.RunStep(ctx, sess, StepRequest{Owner: "tenant-a", AgentSlug: "narrator"}); err != nil {
		t.Fatalf("tenant-a running its own agent: %v", err)
	}

	// A tenant that owns no such agent must not fall through to someone else's.
	if _, err := e.RunStep(ctx, sess, StepRequest{Owner: "tenant-c", AgentSlug: "narrator"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant-c running another tenant's agent: err = %v, want ErrNotFound", err)
	}
	// The global scope is likewise a distinct namespace, not a wildcard.
	if _, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "narrator"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("global scope running an owned agent: err = %v, want ErrNotFound", err)
	}
}

// The cache must be keyed by owner too — otherwise the first tenant to read a
// slug poisons it for every other tenant, which the SQL scoping alone will not
// catch.
func TestG4_CacheIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "g4cache", map[string]Generator{"g": okGen{}}, PollerConfig{})

	if err := e.Agents().Create(ctx, &Agent{
		Slug: "shared", Version: 1, Owner: "tenant-a",
		Modal: ModalityText, GeneratorSlug: "g", Category: "a-cat",
	}); err != nil {
		t.Fatal(err)
	}

	// Prime the cache as tenant-a...
	got, err := e.Agents().Get(ctx, "tenant-a", "shared", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Category != "a-cat" {
		t.Fatalf("Category = %q, want a-cat", got.Category)
	}
	// ...then read the same slug as tenant-b. A shared cache key would serve
	// tenant-a's record here.
	if _, err := e.Agents().Get(ctx, "tenant-b", "shared", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant-b reading tenant-a's cached agent: err = %v, want ErrNotFound", err)
	}
}

// --- B1: placeholders must be usable under a positional $N binder ---

// TestB1_PlaceholdersAppearInAscendingOrder guards the class of bug the client
// hit. loom's own drivers (modernc.org/sqlite, lib/pq) bind $N by NUMBER, so
// out-of-order placeholders are correct here — but callers supply their own
// *sql.DB, and mattn/go-sqlite3 binds $N by ORDER OF APPEARANCE. Any statement
// whose placeholders appear out of numeric order silently binds the wrong
// argument to every position under such a driver, affecting zero rows and
// returning no error.
func TestB1_PlaceholdersAppearInAscendingOrder(t *testing.T) {
	// Walk every Go source file in the module and check each string literal
	// that looks like SQL. Scanning source (rather than enumerating known
	// statements) is what makes this catch the NEXT one someone writes.
	root := ".."
	if wd, err := os.Getwd(); err == nil {
		root = filepath.Join(wd, "..", "..")
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			// A file we cannot parse is not a placeholder failure; skip it.
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s := lit.Value
			if !looksLikeSQL(s) {
				return true
			}
			nums := placeholderNums(s)
			if !ascending(nums) {
				pos := fset.Position(lit.Pos())
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d: $N placeholders appear in order %v, want ascending.\n"+
					"Renumber so appearance order matches numeric order, and reorder the args to match.\n"+
					"Statement: %s", rel, pos.Line, nums, strings.TrimSpace(s))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// looksLikeSQL reports whether a string literal is a SQL statement carrying $N
// placeholders. Deliberately narrow: it only matters for statements that bind
// arguments.
func looksLikeSQL(s string) bool {
	if !strings.Contains(s, "$1") {
		return false
	}
	upper := strings.ToUpper(s)
	for _, kw := range []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE "} {
		if strings.Contains(upper, kw) {
			return true
		}
	}
	return false
}

var placeholderRe = regexp.MustCompile(`\$(\d+)`)

// placeholderNums returns the placeholder numbers in order of appearance.
func placeholderNums(s string) []int {
	var out []int
	for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// ascending reports whether ns is non-decreasing. Repeats are allowed: reusing
// $1 twice is unambiguous under both binding strategies.
func ascending(ns []int) bool {
	for i := 1; i < len(ns); i++ {
		if ns[i] < ns[i-1] {
			return false
		}
	}
	return true
}
