package engine

// gc_service.go wires the gc sub-package into the engine.

import (
	"context"

	loomgc "github.com/rhaqim/loom/gc"
)

// BranchGCConfig is a re-export of gc.Config for callers who import only loom.
type BranchGCConfig = loomgc.Config

// DefaultBranchGCConfig returns sensible production GC defaults.
func DefaultBranchGCConfig() BranchGCConfig {
	return loomgc.DefaultConfig()
}

// NewBranchGCWorker creates a branch GC worker using the engine's DB connection.
// The DB dialect is always sourced from the engine — it is a property of the
// connection, not a caller choice.
func NewBranchGCWorker(e *Engine, cfg BranchGCConfig) *loomgc.Worker {
	cfg.Dialect = string(e.cfg.Dialect)
	return loomgc.NewWorker(e.db, e.prefix, cfg)
}

// gcConfig resolves the GC config: the engine's BranchGC config if set, else
// safe defaults, with the dialect always sourced from the engine.
func (g *gcService) gcConfig() loomgc.Config {
	cfg := loomgc.DefaultConfig()
	if g.e.cfg.BranchGC != nil {
		cfg = *g.e.cfg.BranchGC
	}
	cfg.Dialect = string(g.e.cfg.Dialect)
	return cfg
}

// gcService implements GCService by delegating to a gc.Worker.
// It satisfies the GCService interface declared in engine.go.
func (g *gcService) Run(ctx context.Context) {
	worker := loomgc.NewWorker(g.e.db, g.e.prefix, g.gcConfig())
	worker.Run(ctx)
}

func (g *gcService) DryRun(ctx context.Context) (*SweepReport, error) {
	worker := loomgc.NewWorker(g.e.db, g.e.prefix, g.gcConfig())
	r, err := worker.DryRun(ctx)
	if err != nil {
		return nil, err
	}
	return toSweepReport(r), nil
}

func (g *gcService) Sweep(ctx context.Context) (*SweepReport, error) {
	worker := loomgc.NewWorker(g.e.db, g.e.prefix, g.gcConfig())
	r, err := worker.Sweep(ctx)
	if err != nil {
		// A tier failure still returns the counts from the tiers that ran, so a
		// caller can see what was collected before the error.
		return toSweepReport(r), err
	}
	return toSweepReport(r), nil
}

func toSweepReport(r *loomgc.SweepReport) *SweepReport {
	if r == nil {
		return nil
	}
	return &SweepReport{
		DryRun:              r.DryRun,
		SpeculativeDeleted:  r.SpeculativeDeleted,
		StaleSoftDeleted:    r.StaleSoftDeleted,
		GraceHardDeleted:    r.GraceHardDeleted,
		TestBranchesDeleted: r.TestBranchesDeleted,
	}
}

// SweepReport is what a GC sweep did (or, for DryRun, would have done).
type SweepReport struct {
	// DryRun reports whether these counts describe planned or actual deletions.
	// Without it a caller cannot tell a real sweep's report from a dry one.
	DryRun              bool
	SpeculativeDeleted  int
	StaleSoftDeleted    int
	GraceHardDeleted    int
	TestBranchesDeleted int
}
