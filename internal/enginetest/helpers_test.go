package enginetest

import (
	"context"
	"fmt"
	"math/rand/v2"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/harness"
)

// randomSuffix returns a short random hex string to avoid test slug collisions.
func randomSuffix() string {
	return fmt.Sprintf("%08x", rand.Uint32())
}

// buildHarnessPlan creates a minimal TestPlan for the given agent slug.
func buildHarnessPlan(agentSlug string) *harness.TestPlan {
	return &harness.TestPlan{
		Name: "harness-smoke-" + agentSlug,
		Session: harness.SessionScript{
			PlatformID: "harness-test-user",
			Tags:       []string{"test:true"},
			Steps: []harness.ScriptedStep{
				{AgentSlug: agentSlug, ActionPayload: "Open the dungeon door"},
				{AgentSlug: agentSlug, ActionPayload: "Enter the first chamber"},
			},
		},
		Variants: harness.VariantMatrix{
			Providers: []string{"echo"},
		},
		Assertions: []harness.Assertion{
			harness.HasStatus(loom.ResultStatusReady),
			harness.MinLength(1),
		},
	}
}

// runHarness calls harness.Run with the engine. Kept as a thin wrapper so
// the test file doesn't import harness directly (avoids import cycle check).
func runHarness(ctx context.Context, e *loom.Engine, plan *harness.TestPlan) (*harness.TestReport, error) {
	return harness.Run(ctx, e, plan)
}
