package main

import (
	"context"
	"fmt"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/harness"
)

// TestReport extends harness.TestReport with a top-level Passed field
// so the CLI can exit non-zero on failure.
type TestReport struct {
	*harness.TestReport
	Passed bool
}

func runTestPlan(ctx context.Context, e *loom.Engine, plan *harness.TestPlan) (*TestReport, error) {
	r, err := harness.Run(ctx, e, plan)
	if err != nil {
		return nil, err
	}
	return &TestReport{TestReport: r, Passed: r.Passed()}, nil
}

func printReport(r *TestReport) {
	fmt.Printf("\nTest Plan: %s\n", r.PlanName)
	fmt.Printf("Duration:  %s\n", r.FinishedAt.Sub(r.StartedAt))
	fmt.Println()

	for _, v := range r.Variants {
		icon := "PASS"
		if !v.Passed {
			icon = "FAIL"
		}
		provider := v.Provider
		if provider == "" {
			provider = "(default)"
		}
		fmt.Printf("[%s] variant #%d  provider=%s  duration=%s\n",
			icon, v.Index, provider, v.Duration)

		if v.Err != nil {
			fmt.Printf("       error: %v\n", v.Err)
		}
		for _, sr := range v.Steps {
			stepIcon := "ok"
			if len(sr.Failures) > 0 {
				stepIcon = "FAIL"
			}
			fmt.Printf("       step[%d] agent=%s  result=%s  dur=%dms  [%s]\n",
				sr.StepIndex, sr.AgentSlug, sr.ResultID, sr.DurationMs, stepIcon)
			for _, f := range sr.Failures {
				fmt.Printf("         - %s\n", f)
			}
		}
	}

	fmt.Println()
	if r.Passed {
		fmt.Println("All variants passed.")
	} else {
		fmt.Println("Some variants FAILED.")
	}
}
