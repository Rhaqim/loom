// Package gc provides the branch garbage collection worker.
// It sweeps speculative, stale, and test branches according to a tiered
// retention policy, running daily via a singleton background goroutine.
package gc

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// Config controls the GC retention thresholds.
type Config struct {
	// DryRun logs what would be deleted but takes no action.
	// Ship true on first deployment, flip to false after observing.
	DryRun bool

	// SpeculativeMaxAge is the retention for branches with zero new steps (T1).
	SpeculativeMaxAge time.Duration // default 24h

	// StaleMaxIdle is the idle timeout for branches with new steps (T2).
	StaleMaxIdle time.Duration // default 30 days

	// SoftDeleteGrace is the grace period after soft-delete before hard delete (T3).
	SoftDeleteGrace time.Duration // default 14 days

	// TestBranchMaxAge is the retention for branches tagged "test:true" (T4).
	TestBranchMaxAge time.Duration // default 7 days

	// RunInterval controls how often the sweep runs.
	RunInterval time.Duration // default 24h

	// InitialDelay is how long to wait after boot before the first sweep.
	InitialDelay time.Duration // default 10 minutes

	// Dialect is the database flavour ("postgres" or "sqlite"). It selects
	// dialect-specific SQL (currently the test-tag match, since tags is JSONB on
	// Postgres and TEXT on SQLite). Empty defaults to Postgres syntax.
	Dialect string
}

// DefaultConfig returns sensible production defaults.
func DefaultConfig() Config {
	return Config{
		DryRun:            true, // safe default
		SpeculativeMaxAge: 24 * time.Hour,
		StaleMaxIdle:      30 * 24 * time.Hour,
		SoftDeleteGrace:   14 * 24 * time.Hour,
		TestBranchMaxAge:  7 * 24 * time.Hour,
		RunInterval:       24 * time.Hour,
		InitialDelay:      10 * time.Minute,
	}
}

// SweepReport summarises what was (or would be) deleted in a sweep.
type SweepReport struct {
	SpeculativeDeleted  int // T1 hard-deleted
	StaleSoftDeleted    int // T2 soft-deleted
	GraceHardDeleted    int // T3 hard-deleted
	TestBranchesDeleted int // T4 hard-deleted
	DryRun              bool
}

func (r SweepReport) String() string {
	mode := "live"
	if r.DryRun {
		mode = "dry-run"
	}
	return fmt.Sprintf(
		"GC sweep [%s]: T1=%d T2=%d T3=%d T4=%d",
		mode, r.SpeculativeDeleted, r.StaleSoftDeleted,
		r.GraceHardDeleted, r.TestBranchesDeleted,
	)
}

// Worker runs the periodic GC sweep.
type Worker struct {
	db     *sql.DB
	prefix string
	cfg    Config
}

// NewWorker creates a GC Worker. The config is sanitized so a partially set (or
// zero-value) config cannot turn every age cutoff into now and mass-delete, or
// panic time.NewTicker with a zero interval.
func NewWorker(db *sql.DB, prefix string, cfg Config) *Worker {
	return &Worker{db: db, prefix: prefix, cfg: cfg.sanitized()}
}

// sanitized replaces non-positive durations with DefaultConfig values. DryRun is
// left as the caller set it.
func (c Config) sanitized() Config {
	d := DefaultConfig()
	if c.SpeculativeMaxAge <= 0 {
		c.SpeculativeMaxAge = d.SpeculativeMaxAge
	}
	if c.StaleMaxIdle <= 0 {
		c.StaleMaxIdle = d.StaleMaxIdle
	}
	if c.SoftDeleteGrace <= 0 {
		c.SoftDeleteGrace = d.SoftDeleteGrace
	}
	if c.TestBranchMaxAge <= 0 {
		c.TestBranchMaxAge = d.TestBranchMaxAge
	}
	if c.RunInterval <= 0 {
		c.RunInterval = d.RunInterval
	}
	if c.InitialDelay < 0 {
		c.InitialDelay = d.InitialDelay
	}
	return c
}

// Run starts the GC loop. It blocks until ctx is cancelled.
// Run in a goroutine: go worker.Run(ctx).
func (w *Worker) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(w.cfg.InitialDelay):
	}

	ticker := time.NewTicker(w.cfg.RunInterval)
	defer ticker.Stop()

	for {
		report, err := w.Sweep(ctx)
		if err != nil {
			log.Printf("loom/gc: sweep error: %v", err)
		} else {
			log.Println("loom/gc:", report)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DryRun executes the sweep logic without committing any deletions.
func (w *Worker) DryRun(ctx context.Context) (*SweepReport, error) {
	orig := w.cfg.DryRun
	w.cfg.DryRun = true
	r, err := w.Sweep(ctx)
	w.cfg.DryRun = orig
	return r, err
}

// Sweep executes all four GC tiers in sequence. It is exported so callers (and
// tests) can run a single sweep rather than only the periodic Run loop. Each
// tier deletes in a single statement, so its predicate is re-evaluated
// atomically and a session that gained activity since planning is spared.
func (w *Worker) Sweep(ctx context.Context) (*SweepReport, error) {
	report := &SweepReport{DryRun: w.cfg.DryRun}

	t1, err := w.sweepTier1(ctx)
	if err != nil {
		return report, fmt.Errorf("tier1: %w", err)
	}
	report.SpeculativeDeleted = t1

	t2, err := w.sweepTier2(ctx)
	if err != nil {
		return report, fmt.Errorf("tier2: %w", err)
	}
	report.StaleSoftDeleted = t2

	t3, err := w.sweepTier3(ctx)
	if err != nil {
		return report, fmt.Errorf("tier3: %w", err)
	}
	report.GraceHardDeleted = t3

	t4, err := w.sweepTier4(ctx)
	if err != nil {
		return report, fmt.Errorf("tier4: %w", err)
	}
	report.TestBranchesDeleted = t4

	return report, nil
}

// T1: Speculative branches — a branch that was forked but never actually played.
// A freshly forked branch starts with empty history, so ANY step on the branch
// means it was played and must be preserved. (The previous predicate compared
// step_index to branch_point, which misclassified real branches as speculative
// because a forked branch's step indices restart at 0.)
func (w *Worker) sweepTier1(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-w.cfg.SpeculativeMaxAge)
	p := w.prefix
	where := fmt.Sprintf(`parent_session_id IS NOT NULL
		AND pinned = false
		AND deleted_at IS NULL
		AND created_at < $1
		AND NOT EXISTS (SELECT 1 FROM %ssteps st WHERE st.session_id = %ssessions.id)
		AND NOT EXISTS (SELECT 1 FROM %ssessions child WHERE child.parent_session_id = %ssessions.id AND child.deleted_at IS NULL)`,
		p, p, p, p)
	return w.hardSweep(ctx, where, cutoff)
}

// T2: Stale branches — last activity > StaleMaxIdle, not pinned. Soft-deleted.
func (w *Worker) sweepTier2(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-w.cfg.StaleMaxIdle)
	p := w.prefix
	where := fmt.Sprintf(`parent_session_id IS NOT NULL
		AND pinned = false
		AND deleted_at IS NULL
		AND updated_at < $1
		AND NOT EXISTS (SELECT 1 FROM %ssessions child WHERE child.parent_session_id = %ssessions.id AND child.deleted_at IS NULL)`,
		p, p)
	if w.cfg.DryRun {
		return w.countWhere(ctx, where, cutoff)
	}
	now := time.Now()
	res, err := w.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %ssessions SET deleted_at=$2, updated_at=$3 WHERE %s`, p, where),
		cutoff, now, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// T3: Expired soft-deletes — deleted_at > SoftDeleteGrace, not pinned.
func (w *Worker) sweepTier3(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-w.cfg.SoftDeleteGrace)
	p := w.prefix
	where := fmt.Sprintf(`pinned = false
		AND deleted_at IS NOT NULL AND deleted_at < $1
		AND NOT EXISTS (SELECT 1 FROM %ssessions child WHERE child.parent_session_id = %ssessions.id AND child.deleted_at IS NULL)`,
		p, p)
	return w.hardSweep(ctx, where, cutoff)
}

// T4: Test branches — tagged "test:true", age > TestBranchMaxAge. Scoped to
// branches (parent_session_id IS NOT NULL) and not pinned, so a test-tagged ROOT
// session or a pinned one is never swept.
func (w *Worker) sweepTier4(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-w.cfg.TestBranchMaxAge)
	p := w.prefix
	where := fmt.Sprintf(`parent_session_id IS NOT NULL
		AND pinned = false
		AND deleted_at IS NULL
		AND created_at < $1
		AND NOT EXISTS (SELECT 1 FROM %ssessions child WHERE child.parent_session_id = %ssessions.id AND child.deleted_at IS NULL)
		AND %s`, p, p, w.testTagMatch())
	return w.hardSweep(ctx, where, cutoff)
}

// testTagMatch returns the dialect-appropriate predicate matching the
// "test:true" tag. Tags are stored as a JSON array of strings — JSONB on
// Postgres, TEXT on SQLite. The match tests array *membership*, not a raw
// substring: a substring match (e.g. LIKE '%test:true%') would also match
// unrelated tags such as "latest:true" or "contest:true" and hard-delete
// those sessions. "test:true" is a fixed constant, so it is safe to inline.
func (w *Worker) testTagMatch() string {
	if w.cfg.Dialect == "sqlite" {
		return `EXISTS (SELECT 1 FROM json_each(tags) WHERE value = 'test:true')`
	}
	return `tags @> '["test:true"]'::jsonb`
}

// hardSweep deletes (or, in dry-run, counts) sessions matching where. The delete
// is a single statement so the predicate is re-evaluated atomically: a session
// that gained a new step, child, or activity between planning and deletion no
// longer matches and is spared. This closes the TOCTOU window of a separate
// SELECT-then-per-row-DELETE.
func (w *Worker) hardSweep(ctx context.Context, where string, args ...any) (int, error) {
	if w.cfg.DryRun {
		return w.countWhere(ctx, where, args...)
	}
	res, err := w.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %ssessions WHERE %s`, w.prefix, where), args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (w *Worker) countWhere(ctx context.Context, where string, args ...any) (int, error) {
	var n int
	err := w.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %ssessions WHERE %s`, w.prefix, where), args...).Scan(&n)
	return n, err
}
