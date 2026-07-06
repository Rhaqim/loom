package loom

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
	return &SweepReport{
		SpeculativeDeleted:  r.SpeculativeDeleted,
		StaleSoftDeleted:    r.StaleSoftDeleted,
		GraceHardDeleted:    r.GraceHardDeleted,
		TestBranchesDeleted: r.TestBranchesDeleted,
	}, nil
}
