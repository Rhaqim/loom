// Package judge provides LLM-based scoring as a reusable abstraction. Judges
// evaluate generator outputs along rubric dimensions, compare pairs, or enforce
// binary constraints. They are caller-driven: register a judge and invoke it
// from a test, a post-hook, or agent-selection code. Nothing in the engine
// registers or invokes judges automatically yet.
package judge

import (
	"context"
	"fmt"
)

// Mode identifies the evaluation shape of a Judge.
type Mode string

const (
	ModeRubric     Mode = "rubric"
	ModePairwise   Mode = "pairwise"
	ModeConstraint Mode = "constraint"
)

// Judge is the base interface all judges implement.
type Judge interface {
	Mode() Mode
	Slug() string
}

// -----------------------------------------------------------------------
// Rubric judge — score one output on N dimensions, each 0..10
// -----------------------------------------------------------------------

// ScoreRequest is the input to RubricJudge.Score.
type ScoreRequest struct {
	Input      string   // the prompt the agent received
	Output     string   // the output the agent produced
	Context    []string // optional prior outputs for continuity scoring
	Dimensions []string // e.g. ["coherence","novelty","faithfulness"]
}

// RubricVerdict is the output of RubricJudge.Score.
type RubricVerdict struct {
	Scores       map[string]float64 // dimension → 0..10
	Aggregate    float64            // unweighted mean of all dimension scores
	Explanations map[string]string  // per-dimension one-sentence justification
}

// RubricJudge scores a single output on multiple dimensions.
type RubricJudge interface {
	Judge
	Score(ctx context.Context, req ScoreRequest) (RubricVerdict, error)
}

// -----------------------------------------------------------------------
// Pairwise judge — compare A vs B on each dimension
// -----------------------------------------------------------------------

// CompareRequest is the input to PairwiseJudge.Compare.
type CompareRequest struct {
	Input      string
	OutputA    string
	OutputB    string
	Dimensions []string
}

// PairwiseVerdict is the output of PairwiseJudge.Compare.
type PairwiseVerdict struct {
	Winner       string            // "A" | "B" | "tie"
	PerDimension map[string]string // dimension → "A" | "B" | "tie"
	Margin       float64           // 0..1 confidence
	Explanation  string
}

// PairwiseJudge compares two outputs and picks the stronger one.
type PairwiseJudge interface {
	Judge
	Compare(ctx context.Context, req CompareRequest) (PairwiseVerdict, error)
}

// -----------------------------------------------------------------------
// Constraint judge — binary pass/fail
// -----------------------------------------------------------------------

// CheckRequest is the input to ConstraintJudge.Check.
type CheckRequest struct {
	Input      string
	Output     string
	Constraint string // human-readable rule, e.g. "must not contradict state.Facts"
	Context    []any  // additional context the judge may inspect
}

// ConstraintVerdict is the output of ConstraintJudge.Check.
type ConstraintVerdict struct {
	Passed     bool
	Constraint string
	Violations []string
	Confidence float64
}

// ConstraintJudge checks whether an output satisfies a stated rule.
type ConstraintJudge interface {
	Judge
	Check(ctx context.Context, req CheckRequest) (ConstraintVerdict, error)
}

// -----------------------------------------------------------------------
// Registry
// -----------------------------------------------------------------------

// Registry holds all registered judges keyed by slug.
type Registry struct {
	judges map[string]Judge
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{judges: make(map[string]Judge)}
}

// Register adds a judge under its intrinsic Slug().
func (r *Registry) Register(j Judge) {
	r.judges[j.Slug()] = j
}

// RegisterAs adds a judge under an explicit slug, which may differ from the
// judge's intrinsic Slug(). Lookups use this key.
func (r *Registry) RegisterAs(slug string, j Judge) {
	r.judges[slug] = j
}

// Rubric returns the RubricJudge registered under slug, or a no-op judge.
func (r *Registry) Rubric(slug string) RubricJudge {
	if j, ok := r.judges[slug]; ok {
		if rj, ok := j.(RubricJudge); ok {
			return rj
		}
	}
	return &noopRubricJudge{slug: slug}
}

// Pairwise returns the PairwiseJudge registered under slug, or a no-op judge.
func (r *Registry) Pairwise(slug string) PairwiseJudge {
	if j, ok := r.judges[slug]; ok {
		if pj, ok := j.(PairwiseJudge); ok {
			return pj
		}
	}
	return &noopPairwiseJudge{slug: slug}
}

// Constraint returns the ConstraintJudge registered under slug, or a no-op judge.
func (r *Registry) Constraint(slug string) ConstraintJudge {
	if j, ok := r.judges[slug]; ok {
		if cj, ok := j.(ConstraintJudge); ok {
			return cj
		}
	}
	return &noopConstraintJudge{slug: slug}
}

// -----------------------------------------------------------------------
// No-op implementations (returned when slug not found)
// -----------------------------------------------------------------------

type noopRubricJudge struct{ slug string }

func (j *noopRubricJudge) Mode() Mode   { return ModeRubric }
func (j *noopRubricJudge) Slug() string { return j.slug }
func (j *noopRubricJudge) Score(_ context.Context, req ScoreRequest) (RubricVerdict, error) {
	v := RubricVerdict{
		Scores:       make(map[string]float64),
		Explanations: make(map[string]string),
	}
	for _, d := range req.Dimensions {
		v.Scores[d] = 5.0
		v.Explanations[d] = fmt.Sprintf("judge %q not found; returning neutral score", j.slug)
	}
	v.Aggregate = 5.0
	return v, nil
}

type noopPairwiseJudge struct{ slug string }

func (j *noopPairwiseJudge) Mode() Mode   { return ModePairwise }
func (j *noopPairwiseJudge) Slug() string { return j.slug }
func (j *noopPairwiseJudge) Compare(_ context.Context, _ CompareRequest) (PairwiseVerdict, error) {
	return PairwiseVerdict{
		Winner:       "tie",
		PerDimension: map[string]string{},
		Margin:       0,
		Explanation:  fmt.Sprintf("judge %q not found; returning tie", j.slug),
	}, nil
}

type noopConstraintJudge struct{ slug string }

func (j *noopConstraintJudge) Mode() Mode   { return ModeConstraint }
func (j *noopConstraintJudge) Slug() string { return j.slug }
func (j *noopConstraintJudge) Check(_ context.Context, req CheckRequest) (ConstraintVerdict, error) {
	// A judge that never ran must not assert certainty: report zero confidence
	// and flag that the check was skipped, so callers gating on confidence don't
	// treat an unregistered constraint as a confident pass.
	return ConstraintVerdict{
		Passed:     true,
		Constraint: req.Constraint,
		Violations: []string{fmt.Sprintf("judge %q not found; constraint not checked", j.slug)},
		Confidence: 0,
	}, nil
}
