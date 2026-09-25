package evaluator_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/rhaqim/loom/evaluator"
	"github.com/rhaqim/loom/evaluator/stub"
	"github.com/rhaqim/loom/judge"
)

// The adapters must satisfy the existing judge interfaces exactly — that is
// what makes registering one a swap rather than a migration.
var (
	_ judge.RubricJudge     = (*evaluator.RubricJudge)(nil)
	_ judge.ConstraintJudge = (*evaluator.ConstraintJudge)(nil)
	_ judge.PairwiseJudge   = (*evaluator.PairwiseJudge)(nil)
)

func TestRubricJudgeRescalesOntoJudgeRange(t *testing.T) {
	// The default rubric has 5 levels (span 4), so a top-level answer must
	// arrive as judge's 10 and the midpoint as 5.
	ev := stub.New().WithScore("coherence", 4).WithScore("novelty", 2)
	j := evaluator.NewRubricJudge("q", ev)

	v, err := j.Score(context.Background(), judge.ScoreRequest{
		Input:      "prompt",
		Output:     "draft",
		Dimensions: []string{"coherence", "novelty"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Scores["coherence"] != 10 {
		t.Errorf("coherence = %v, want 10", v.Scores["coherence"])
	}
	if v.Scores["novelty"] != 5 {
		t.Errorf("novelty = %v, want 5", v.Scores["novelty"])
	}
	if v.Aggregate != 7.5 {
		t.Errorf("Aggregate = %v, want 7.5", v.Aggregate)
	}
	if !strings.Contains(v.Explanations["coherence"], "weighted") {
		t.Errorf("explanation = %q, want it to quote the numbers behind the score", v.Explanations["coherence"])
	}
}

func TestRubricJudgeAsksOneQuestionPerDimension(t *testing.T) {
	// Independence is the reason to prefer this over the LLM rubric judge: each
	// dimension must be its own question, in ONE call.
	ev := stub.New()
	j := evaluator.NewRubricJudge("q", ev)
	if _, err := j.Score(context.Background(), judge.ScoreRequest{
		Output:     "draft",
		Dimensions: []string{"a", "b", "c"},
	}); err != nil {
		t.Fatal(err)
	}
	calls := ev.Calls()
	if len(calls) != 1 {
		t.Fatalf("made %d evaluations, want 1", len(calls))
	}
	if len(calls[0].Questions) != 3 {
		t.Fatalf("asked %d questions, want 3", len(calls[0].Questions))
	}
	for _, d := range []string{"a", "b", "c"} {
		if calls[0].Questions[d].Type != evaluator.TypeScore {
			t.Errorf("dimension %q is a %s question, want score", d, calls[0].Questions[d].Type)
		}
	}
}

func TestRubricJudgeCustomLevels(t *testing.T) {
	// A 3-level rubric has span 2, so level 1 is the midpoint and must rescale
	// to 5 — the rescaling has to follow the rubric, not a fixed assumption.
	ev := stub.New().WithScore("tone", 1)
	j := evaluator.NewRubricJudge("q", ev).WithLevels("bad", "ok", "good")
	v, err := j.Score(context.Background(), judge.ScoreRequest{Output: "d", Dimensions: []string{"tone"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Scores["tone"] != 5 {
		t.Errorf("tone = %v, want 5", v.Scores["tone"])
	}
}

func TestRubricJudgeRejectsMalformedLevels(t *testing.T) {
	// Silently scoring against a one-level rubric would be worse than ignoring
	// the call, so the levels must be left alone.
	ev := stub.New().WithScore("tone", 4)
	j := evaluator.NewRubricJudge("q", ev).WithLevels("only")
	v, err := j.Score(context.Background(), judge.ScoreRequest{Output: "d", Dimensions: []string{"tone"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Scores["tone"] != 10 {
		t.Errorf("tone = %v, want 10 (the default 5-level rubric should still be in force)", v.Scores["tone"])
	}
}

func TestRubricJudgeRequiresDimensions(t *testing.T) {
	_, err := evaluator.NewRubricJudge("q", stub.New()).
		Score(context.Background(), judge.ScoreRequest{Output: "d"})
	if err == nil {
		t.Fatal("Score() with no dimensions = nil error, want a failure")
	}
}

func TestRubricJudgePropagatesEvaluatorError(t *testing.T) {
	boom := errors.New("provider down")
	_, err := evaluator.NewRubricJudge("q", stub.New().WithError(boom)).
		Score(context.Background(), judge.ScoreRequest{Output: "d", Dimensions: []string{"a"}})
	if !errors.Is(err, boom) {
		t.Fatalf("Score() = %v, want it to wrap the evaluator error", err)
	}
}

func TestConstraintJudgePassesAndFails(t *testing.T) {
	ctx := context.Background()
	j := evaluator.NewConstraintJudge("c", stub.New().WithNoul("satisfied", 0.95))
	v, err := j.Check(ctx, judge.CheckRequest{Output: "text", Constraint: "must not contradict the facts"})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Passed {
		t.Error("Passed = false at p=0.95, want true")
	}
	if math.Abs(v.Confidence-0.9) > 1e-9 {
		t.Errorf("Confidence = %v, want 0.9 derived from the probability", v.Confidence)
	}
	if len(v.Violations) != 0 {
		t.Errorf("Violations = %v, want none on a pass", v.Violations)
	}

	j = evaluator.NewConstraintJudge("c", stub.New().WithNoul("satisfied", 0.3))
	v, err = j.Check(ctx, judge.CheckRequest{Output: "text", Constraint: "must not contradict the facts"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Passed {
		t.Error("Passed = true at p=0.3, want false")
	}
	if len(v.Violations) != 1 || !strings.Contains(v.Violations[0], "0.30") {
		t.Errorf("Violations = %v, want one quoting the probability that drove the verdict", v.Violations)
	}
}

func TestConstraintJudgeThresholdIsTunable(t *testing.T) {
	ctx := context.Background()
	req := judge.CheckRequest{Output: "text", Constraint: "rule"}
	ev := stub.New().WithNoul("satisfied", 0.6)

	// The default 0.7 threshold fails 0.6; lowering it to 0.5 must pass it.
	if v, _ := evaluator.NewConstraintJudge("c", ev).Check(ctx, req); v.Passed {
		t.Error("Passed = true at p=0.6 under the default 0.7 threshold, want false")
	}
	if v, _ := evaluator.NewConstraintJudge("c", ev).WithThreshold(0.5).Check(ctx, req); !v.Passed {
		t.Error("Passed = false at p=0.6 under a 0.5 threshold, want true")
	}
	// An out-of-range threshold is ignored rather than silently applied.
	if v, _ := evaluator.NewConstraintJudge("c", ev).WithThreshold(2).Check(ctx, req); v.Passed {
		t.Error("an out-of-range threshold should leave the default in force")
	}
}

func TestConstraintJudgeRequiresAConstraint(t *testing.T) {
	_, err := evaluator.NewConstraintJudge("c", stub.New()).
		Check(context.Background(), judge.CheckRequest{Output: "text", Constraint: "  "})
	if err == nil {
		t.Fatal("Check() with a blank constraint = nil error, want a failure")
	}
}

func TestPairwiseJudgeComparesOverallAndPerDimension(t *testing.T) {
	ev := stub.New()
	j := evaluator.NewPairwiseJudge("p", ev)
	// The overall question's id is internal, so drive it through the stub's
	// default: an even three-way spread that names the first sorted option.
	v, err := j.Compare(context.Background(), judge.CompareRequest{
		OutputA:    "a",
		OutputB:    "b",
		Dimensions: []string{"vividness", "pacing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Winner != "A" {
		t.Errorf("Winner = %q, want A (the stub's deterministic first option)", v.Winner)
	}
	// A three-way toss-up must report a small margin, not a confident verdict.
	if v.Margin > 0.34 {
		t.Errorf("Margin = %v, want a low margin on an even spread", v.Margin)
	}
	if len(v.PerDimension) != 2 {
		t.Errorf("PerDimension = %v, want both dimensions", v.PerDimension)
	}
	calls := ev.Calls()
	if len(calls) != 1 {
		t.Fatalf("made %d evaluations, want 1 covering overall plus both dimensions", len(calls))
	}
	if len(calls[0].Questions) != 3 {
		t.Fatalf("asked %d questions, want 3 (overall + 2 dimensions)", len(calls[0].Questions))
	}
}

func TestPairwiseJudgeWorksWithNoDimensions(t *testing.T) {
	v, err := evaluator.NewPairwiseJudge("p", stub.New()).
		Compare(context.Background(), judge.CompareRequest{OutputA: "a", OutputB: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Winner == "" {
		t.Error("Winner = \"\", want the overall comparison to still run")
	}
}

func TestJudgesRegisterInTheJudgeRegistry(t *testing.T) {
	// The adapters must be usable through the existing registry, since that is
	// how every current call site reaches a judge.
	ev := stub.New().WithNoul("satisfied", 0.9)
	reg := judge.NewRegistry()
	reg.RegisterAs("rules", evaluator.NewConstraintJudge("rules", ev))

	v, err := reg.Constraint("rules").Check(context.Background(),
		judge.CheckRequest{Output: "text", Constraint: "rule"})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Passed {
		t.Error("the registered evaluator-backed judge did not answer; a no-op judge was returned instead")
	}
}
