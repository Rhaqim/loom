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

	"github.com/google/uuid"
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

// NewWorker creates a GC Worker.
func NewWorker(db *sql.DB, prefix string, cfg Config) *Worker {
	return &Worker{db: db, prefix: prefix, cfg: cfg}
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
		report, err := w.sweep(ctx)
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
	r, err := w.sweep(ctx)
	w.cfg.DryRun = orig
	return r, err
}

// sweep executes all four GC tiers in sequence.
// Each tier runs in its own transaction so a failure in tier N doesn't
// roll back successful work in earlier tiers.
func (w *Worker) sweep(ctx context.Context) (*SweepReport, error) {
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

// T1: Speculative branches — 0 new steps beyond the fork point, age > SpeculativeMaxAge.
func (w *Worker) sweepTier1(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-w.cfg.SpeculativeMaxAge)
	p := w.prefix

	// Find branch sessions with no steps beyond their branch_point.
	rows, err := w.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT s.id
		FROM %ssessions s
		WHERE s.parent_session_id IS NOT NULL
		  AND s.pinned = false
		  AND s.deleted_at IS NULL
		  AND s.created_at < $1
		  AND NOT EXISTS (
		      SELECT 1 FROM %ssteps st
		      WHERE st.session_id = s.id
		        AND (s.branch_point IS NULL OR st.step_index > s.branch_point)
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM %ssessions child
		      WHERE child.parent_session_id = s.id AND child.deleted_at IS NULL
		  )`, p, p, p), cutoff)
	if err != nil {
		return 0, err
	}
	return w.hardDeleteRows(ctx, rows)
}

// T2: Stale branches — last activity > StaleMaxIdle, not pinned.
func (w *Worker) sweepTier2(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-w.cfg.StaleMaxIdle)
	p := w.prefix

	rows, err := w.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM %ssessions
		WHERE parent_session_id IS NOT NULL
		  AND pinned = false
		  AND deleted_at IS NULL
		  AND updated_at < $1
		  AND NOT EXISTS (
		      SELECT 1 FROM %ssessions child
		      WHERE child.parent_session_id = %ssessions.id AND child.deleted_at IS NULL
		  )`, p, p, p), cutoff)
	if err != nil {
		return 0, err
	}
	if w.cfg.DryRun {
		return countRows(rows), nil
	}
	now := time.Now()
	return w.softDeleteRows(ctx, rows, now)
}

// T3: Expired soft-deletes — deleted_at > SoftDeleteGrace.
func (w *Worker) sweepTier3(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-w.cfg.SoftDeleteGrace)
	p := w.prefix

	rows, err := w.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM %ssessions
		WHERE deleted_at IS NOT NULL AND deleted_at < $1
		  AND NOT EXISTS (
		      SELECT 1 FROM %ssessions child
		      WHERE child.parent_session_id = %ssessions.id AND child.deleted_at IS NULL
		  )`, p, p, p), cutoff)
	if err != nil {
		return 0, err
	}
	return w.hardDeleteRows(ctx, rows)
}

// T4: Test branches — tagged "test:true", age > TestBranchMaxAge.
func (w *Worker) sweepTier4(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-w.cfg.TestBranchMaxAge)
	p := w.prefix

	rows, err := w.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM %ssessions
		WHERE deleted_at IS NULL
		  AND created_at < $1
		  AND tags::text LIKE '%%test:true%%'`, p), cutoff)
	if err != nil {
		return 0, err
	}
	return w.hardDeleteRows(ctx, rows)
}

func (w *Worker) hardDeleteRows(ctx context.Context, rows *sql.Rows) (int, error) {
	defer rows.Close()
	ids, err := collectIDs(rows)
	if err != nil {
		return 0, err
	}
	if w.cfg.DryRun {
		return len(ids), nil
	}
	count := 0
	for _, id := range ids {
		_, err := w.db.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %ssessions WHERE id=$1`, w.prefix), id)
		if err != nil {
			log.Printf("loom/gc: hard-delete session %s: %v", id, err)
			continue
		}
		count++
	}
	return count, nil
}

func (w *Worker) softDeleteRows(ctx context.Context, rows *sql.Rows, now time.Time) (int, error) {
	ids, err := collectIDs(rows)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		_, err := w.db.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %ssessions SET deleted_at=$2, updated_at=$3 WHERE id=$1`, w.prefix),
			id, now, now)
		if err != nil {
			log.Printf("loom/gc: soft-delete session %s: %v", id, err)
			continue
		}
		count++
	}
	return count, nil
}

func collectIDs(rows *sql.Rows) ([]uuid.UUID, error) {
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func countRows(rows *sql.Rows) int {
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n
}
