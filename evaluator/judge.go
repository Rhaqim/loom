package evaluator

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rhaqim/loom/judge"
)

// judge.go adapts an Evaluator to loom's three judge interfaces. The mapping is
// one-to-one and was not contrived for it — the judge modes and the question
// types describe the same three decisions:
//
//	judge.RubricJudge     → one Score question per dimension
//	judge.ConstraintJudge → one Noul question
//	judge.PairwiseJudge   → one Choice question (A / B / tie) per dimension
//
// The win over the LLM-backed judges in judge/llm.go is not just cost. Those
// send every dimension in one prompt and parse a JSON object back, so the
// dimensions contaminate each other and the reported confidence is a number the
// model asserts. Here each dimension is an independent question and the
// confidence is derived from an actual probability distribution.
//
// These adapters are EXPERIMENTAL along with the rest of the package. They
// satisfy the existing judge interfaces exactly, so registering one is a
// swap at the call site and nothing downstream changes:
//
//	e.Judges().Register("quality", evaluator.NewRubricJudge("quality", ev))

// defaultRubricLevels is the generic 5-level scale used when a rubric judge is
// built without explicit levels. It describes concrete situations rather than
// abstract degrees, which is what makes levels separate cleanly. Supply your
// own with NewRubricJudge(...).WithLevels for anything domain-specific — a
// generic scale is a fallback, not a good rubric.
var defaultRubricLevels = []string{
	"Fails at the task: wrong, empty, or unusable.",
	"Attempts the task but has clear defects a reader would notice immediately.",
	"Adequate: does the job, with nothing notable for or against it.",
	"Good: does the job well, with evident care in the details.",
	"Excellent: does the job as well as this could reasonably be done.",
}

// -----------------------------------------------------------------------
// Rubric
// -----------------------------------------------------------------------

// RubricJudge scores an output on each requested dimension by asking the
// Evaluator one Score question per dimension. It satisfies judge.RubricJudge.
type RubricJudge struct {
	slug   string
	ev     Evaluator
	levels []string
}

// NewRubricJudge builds a rubric judge backed by ev, using the default generic
// 5-level scale. Call WithLevels to supply a rubric that actually describes
// your domain.
func NewRubricJudge(slug string, ev Evaluator) *RubricJudge {
	return &RubricJudge{slug: slug, ev: ev, levels: defaultRubricLevels}
}

// WithLevels replaces the rubric levels, ordered worst to best. Two to ten
// levels are allowed; anything else leaves the existing levels in place, since
// a judge that silently scored against a malformed rubric would be worse than
// one that ignored the call.
func (j *RubricJudge) WithLevels(levels ...string) *RubricJudge {
	if len(levels) >= 2 && len(levels) <= 10 {
		j.levels = levels
	}
	return j
}

func (j *RubricJudge) Mode() judge.Mode { return judge.ModeRubric }
func (j *RubricJudge) Slug() string     { return j.slug }

// Score rates the output on every requested dimension in a single evaluation,
// rescaling each answer onto judge's 0..10 range. The explanation for each
// dimension names the level the answer landed on and how sure the model was —
// derived from the distribution rather than asserted by a model, so it cannot
// disagree with the score it explains.
func (j *RubricJudge) Score(ctx context.Context, req judge.ScoreRequest) (judge.RubricVerdict, error) {
	if len(req.Dimensions) == 0 {
		return judge.RubricVerdict{}, fmt.Errorf("judge %q: no dimensions to score", j.slug)
	}
	questions := make(map[string]Question, len(req.Dimensions))
	for _, d := range req.Dimensions {
		questions[d] = Score(fmt.Sprintf("Rate the OUTPUT on this dimension: %s", d), j.levels...)
	}
	eval, err := j.ev.Evaluate(ctx, rubricState(req), questions)
	if err != nil {
		return judge.RubricVerdict{}, fmt.Errorf("judge %q: evaluate: %w", j.slug, err)
	}

	v := judge.RubricVerdict{
		Scores:       make(map[string]float64, len(req.Dimensions)),
		Explanations: make(map[string]string, len(req.Dimensions)),
	}
	// span is the distance between the lowest and highest level, so a rubric of
	// any length rescales onto judge's fixed 0..10 range.
	span := float64(len(j.levels) - 1)
	var sum float64
	var scored int
	for _, d := range req.Dimensions {
		a, ok := eval.Answers[d]
		if !ok {
			// An Evaluator that drops a question is broken, but a missing
			// dimension must not silently read as a neutral pass.
			v.Scores[d] = 5.0
			v.Explanations[d] = "dimension missing from evaluation; defaulted to neutral"
			sum += 5.0
			continue
		}
		s := a.Score / span * 10
		v.Scores[d] = clamp(s, 0, 10)
		v.Explanations[d] = explainLevel(a)
		sum += v.Scores[d]
		scored++
	}
	if scored == 0 {
		return judge.RubricVerdict{}, fmt.Errorf("judge %q: evaluation answered none of the requested dimensions", j.slug)
	}
	v.Aggregate = sum / float64(len(req.Dimensions))
	return v, nil
}

// rubricState packages the judged material as structured state. The input and
// any continuity context travel alongside the output as named fields rather
// than being concatenated into one blob, so a question can refer to exactly the
// part it is about.
func rubricState(req judge.ScoreRequest) any {
	state := map[string]any{"output": req.Output}
	if req.Input != "" {
		state["input"] = req.Input
	}
	if len(req.Context) > 0 {
		state["prior_outputs"] = req.Context
	}
	return state
}

// explainLevel describes where a Score answer landed, using the rubric legend
// the evaluator echoed back.
func explainLevel(a Answer) string {
	desc := a.Legend[fmt.Sprintf("%d", a.Level())]
	if desc == "" {
		desc = fmt.Sprintf("level %d", a.Level())
	}
	return fmt.Sprintf("%s (weighted %.2f, confidence %.2f)", desc, a.Score, a.Confidence)
}

// -----------------------------------------------------------------------
// Constraint
// -----------------------------------------------------------------------

// defaultConstraintThreshold is the yes-probability at which a constraint counts
// as satisfied. It sits above 0.5 because a constraint check is asymmetric: the
// cost of waving through a violation is normally higher than the cost of a
// spurious retry.
const defaultConstraintThreshold = 0.7

// ConstraintJudge checks an output against a stated rule by asking the
// Evaluator a single Noul question. It satisfies judge.ConstraintJudge.
type ConstraintJudge struct {
	slug      string
	ev        Evaluator
	threshold float64
}

// NewConstraintJudge builds a constraint judge backed by ev, passing at a
// yes-probability of 0.7 or above.
func NewConstraintJudge(slug string, ev Evaluator) *ConstraintJudge {
	return &ConstraintJudge{slug: slug, ev: ev, threshold: defaultConstraintThreshold}
}

// WithThreshold sets the yes-probability at which the constraint counts as
// satisfied. Raise it when letting a violation through is expensive, lower it
// when a spurious failure is. Values outside 0..1 are ignored.
func (j *ConstraintJudge) WithThreshold(t float64) *ConstraintJudge {
	if t >= 0 && t <= 1 {
		j.threshold = t
	}
	return j
}

func (j *ConstraintJudge) Mode() judge.Mode { return judge.ModeConstraint }
func (j *ConstraintJudge) Slug() string     { return j.slug }

// Check asks whether the output satisfies the constraint. Unlike the LLM-backed
// constraint judge, the reported Confidence is derived from the probability
// rather than asserted by the model, so a borderline call reports itself as
// borderline — callers gating on confidence can route those cases elsewhere
// instead of treating a coin flip as a verdict.
func (j *ConstraintJudge) Check(ctx context.Context, req judge.CheckRequest) (judge.ConstraintVerdict, error) {
	if strings.TrimSpace(req.Constraint) == "" {
		return judge.ConstraintVerdict{}, fmt.Errorf("judge %q: no constraint to check", j.slug)
	}
	state := map[string]any{"output": req.Output}
	if req.Input != "" {
		state["input"] = req.Input
	}
	if len(req.Context) > 0 {
		state["context"] = req.Context
	}
	q := NoulWith(
		fmt.Sprintf("Does the OUTPUT satisfy this rule: %s", req.Constraint),
		"The output fully satisfies the rule.",
		"The output violates the rule in at least one place.",
	)
	eval, err := j.ev.Evaluate(ctx, state, map[string]Question{"satisfied": q})
	if err != nil {
		return judge.ConstraintVerdict{}, fmt.Errorf("judge %q: evaluate: %w", j.slug, err)
	}
	a, ok := eval.Answers["satisfied"]
	if !ok {
		return judge.ConstraintVerdict{}, fmt.Errorf("judge %q: evaluation returned no answer", j.slug)
	}

	v := judge.ConstraintVerdict{
		Passed:     a.Yes(j.threshold),
		Constraint: req.Constraint,
		Confidence: a.Certainty(),
	}
	if !v.Passed {
		// There is no free-text explanation to quote, so report the number that
		// actually drove the verdict rather than inventing a reason for it.
		v.Violations = []string{fmt.Sprintf(
			"constraint not satisfied: yes-probability %.2f is below the %.2f threshold",
			a.Noul, j.threshold)}
	}
	return v, nil
}

// -----------------------------------------------------------------------
// Pairwise
// -----------------------------------------------------------------------

const (
	sideA   = "A"
	sideB   = "B"
	sideTie = "tie"
	// overallKey is the question id for the whole-output comparison, kept
	// distinct from any dimension name a caller might pass.
	overallKey = "__overall__"
)

// PairwiseJudge compares two outputs by asking the Evaluator a Choice question
// over A / B / tie — one for the outputs as a whole, plus one per requested
// dimension, all in a single evaluation. It satisfies judge.PairwiseJudge.
type PairwiseJudge struct {
	slug string
	ev   Evaluator
}

// NewPairwiseJudge builds a pairwise judge backed by ev.
func NewPairwiseJudge(slug string, ev Evaluator) *PairwiseJudge {
	return &PairwiseJudge{slug: slug, ev: ev}
}

func (j *PairwiseJudge) Mode() judge.Mode { return judge.ModePairwise }
func (j *PairwiseJudge) Slug() string     { return j.slug }

// Compare picks the stronger of two outputs. Margin is the overall answer's
// confidence, which — unlike the LLM judge's — is a real property of the
// distribution: a genuine toss-up reports a margin near zero rather than a
// confidently-stated tie.
//
// Dimensions are optional; with none supplied only the overall comparison runs.
func (j *PairwiseJudge) Compare(ctx context.Context, req judge.CompareRequest) (judge.PairwiseVerdict, error) {
	state := map[string]any{"a": req.OutputA, "b": req.OutputB}
	if req.Input != "" {
		state["input"] = req.Input
	}
	sides := map[string]any{
		sideA:   "Output A is better.",
		sideB:   "Output B is better.",
		sideTie: "The two are equally good; neither is clearly better.",
	}

	questions := map[string]Question{
		overallKey: Choice("Which output is better overall?", sides),
	}
	for _, d := range req.Dimensions {
		if d == overallKey {
			continue
		}
		questions[d] = Choice(fmt.Sprintf("Which output is better on this dimension: %s", d), sides)
	}

	eval, err := j.ev.Evaluate(ctx, state, questions)
	if err != nil {
		return judge.PairwiseVerdict{}, fmt.Errorf("judge %q: evaluate: %w", j.slug, err)
	}
	overall, ok := eval.Answers[overallKey]
	if !ok {
		return judge.PairwiseVerdict{}, fmt.Errorf("judge %q: evaluation returned no overall comparison", j.slug)
	}

	v := judge.PairwiseVerdict{
		Winner:       overall.Choice,
		PerDimension: make(map[string]string, len(req.Dimensions)),
		Margin:       overall.Confidence,
	}
	for _, d := range req.Dimensions {
		if a, ok := eval.Answers[d]; ok {
			v.PerDimension[d] = a.Choice
		}
	}
	v.Explanation = explainPairwise(overall, v.PerDimension)
	return v, nil
}

// explainPairwise states the overall distribution and any dimensions that went
// against the winner — the disagreements are the part worth reading.
func explainPairwise(overall Answer, perDim map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "overall %s (A %.2f / B %.2f / tie %.2f)",
		overall.Choice, overall.P(sideA), overall.P(sideB), overall.P(sideTie))

	var dissent []string
	for d, w := range perDim {
		if w != overall.Choice {
			dissent = append(dissent, fmt.Sprintf("%s→%s", d, w))
		}
	}
	if len(dissent) > 0 {
		sort.Strings(dissent)
		fmt.Fprintf(&b, "; dissenting dimensions: %s", strings.Join(dissent, ", "))
	}
	return b.String()
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
