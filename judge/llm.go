package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Completer is the minimal text-generation capability a judge needs. It is kept
// deliberately narrow so the judge package carries no engine or provider
// dependency; adapt a concrete generator to it at the call site.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// -----------------------------------------------------------------------
// LLM-backed rubric judge
// -----------------------------------------------------------------------

const rubricSystem = `You are a strict evaluation judge. Score the OUTPUT against each requested dimension on a 0 to 10 scale, where 0 is terrible and 10 is excellent. Respond with ONLY a single JSON object, no prose and no code fences, of exactly this shape:
{"scores": {"<dimension>": <number 0-10>, ...}, "explanations": {"<dimension>": "<one sentence>", ...}}
Include every requested dimension as a key in both objects.`

// LLMRubricJudge scores a single output across dimensions by prompting a model.
type LLMRubricJudge struct {
	slug string
	c    Completer
}

// NewLLMRubricJudge builds a rubric judge backed by the given completer.
func NewLLMRubricJudge(slug string, c Completer) *LLMRubricJudge {
	return &LLMRubricJudge{slug: slug, c: c}
}

func (j *LLMRubricJudge) Mode() Mode   { return ModeRubric }
func (j *LLMRubricJudge) Slug() string { return j.slug }

// Score asks the model to rate the output on each requested dimension. It errors
// if the reply carries no score for any requested dimension (a wrong-shaped reply
// must not masquerade as a neutral verdict). Dimensions present but individually
// unparseable, or genuinely omitted while others were scored, default to a
// neutral 5.0; out-of-range scores are clamped to 0..10. The aggregate is the
// mean over the requested dimensions.
func (j *LLMRubricJudge) Score(ctx context.Context, req ScoreRequest) (RubricVerdict, error) {
	if len(req.Dimensions) == 0 {
		return RubricVerdict{}, fmt.Errorf("judge %q: no dimensions to score", j.slug)
	}
	raw, err := j.c.Complete(ctx, rubricSystem, buildRubricUser(req))
	if err != nil {
		return RubricVerdict{}, fmt.Errorf("judge %q: complete: %w", j.slug, err)
	}
	var parsed struct {
		Scores       map[string]json.RawMessage `json:"scores"`
		Explanations map[string]string          `json:"explanations"`
	}
	if err := decodeFirstJSON(raw, &parsed); err != nil {
		return RubricVerdict{}, fmt.Errorf("judge %q: parse verdict: %w", j.slug, err)
	}

	present := 0
	for _, d := range req.Dimensions {
		if _, ok := parsed.Scores[d]; ok {
			present++
		}
	}
	if present == 0 {
		return RubricVerdict{}, fmt.Errorf("judge %q: response contained no requested-dimension scores", j.slug)
	}

	v := RubricVerdict{
		Scores:       make(map[string]float64, len(req.Dimensions)),
		Explanations: make(map[string]string, len(req.Dimensions)),
	}
	var sum float64
	for _, d := range req.Dimensions {
		rawScore, ok := parsed.Scores[d]
		if !ok {
			v.Scores[d] = 5.0
			v.Explanations[d] = "dimension missing from judge response; defaulted to neutral"
			sum += 5.0
			continue
		}
		score, ok := looseFloat(rawScore)
		if !ok {
			v.Scores[d] = 5.0
			v.Explanations[d] = "dimension score unparseable; defaulted to neutral"
			sum += 5.0
			continue
		}
		score = clamp(score, 0, 10)
		v.Scores[d] = score
		if ex, ok := parsed.Explanations[d]; ok {
			v.Explanations[d] = ex
		}
		sum += score
	}
	v.Aggregate = sum / float64(len(req.Dimensions))
	return v, nil
}

func buildRubricUser(req ScoreRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DIMENSIONS: %s\n\n", strings.Join(req.Dimensions, ", "))
	if req.Input != "" {
		fmt.Fprintf(&b, "INPUT:\n%s\n\n", req.Input)
	}
	for i, c := range req.Context {
		fmt.Fprintf(&b, "PRIOR OUTPUT %d:\n%s\n\n", i+1, c)
	}
	fmt.Fprintf(&b, "OUTPUT TO SCORE:\n%s", req.Output)
	return b.String()
}

// -----------------------------------------------------------------------
// LLM-backed constraint judge
// -----------------------------------------------------------------------

const constraintSystem = `You are a strict constraint checker. Decide whether the OUTPUT satisfies the stated CONSTRAINT. Respond with ONLY a single JSON object, no prose and no code fences, of exactly this shape:
{"passed": <true|false>, "violations": ["<short description>", ...], "confidence": <number 0-1>}
List violations only when passed is false; use an empty array when the output passes.`

// LLMConstraintJudge enforces a single binary rule by prompting a model.
type LLMConstraintJudge struct {
	slug string
	c    Completer
}

// NewLLMConstraintJudge builds a constraint judge backed by the given completer.
func NewLLMConstraintJudge(slug string, c Completer) *LLMConstraintJudge {
	return &LLMConstraintJudge{slug: slug, c: c}
}

func (j *LLMConstraintJudge) Mode() Mode   { return ModeConstraint }
func (j *LLMConstraintJudge) Slug() string { return j.slug }

// Check asks the model whether the output satisfies the constraint. A reply that
// omits the "passed" verdict is an error rather than a synthesized failure.
func (j *LLMConstraintJudge) Check(ctx context.Context, req CheckRequest) (ConstraintVerdict, error) {
	if strings.TrimSpace(req.Constraint) == "" {
		return ConstraintVerdict{}, fmt.Errorf("judge %q: empty constraint", j.slug)
	}
	raw, err := j.c.Complete(ctx, constraintSystem, buildConstraintUser(req))
	if err != nil {
		return ConstraintVerdict{}, fmt.Errorf("judge %q: complete: %w", j.slug, err)
	}
	var parsed struct {
		Passed     *bool           `json:"passed"`
		Violations []string        `json:"violations"`
		Confidence json.RawMessage `json:"confidence"`
	}
	if err := decodeFirstJSON(raw, &parsed); err != nil {
		return ConstraintVerdict{}, fmt.Errorf("judge %q: parse verdict: %w", j.slug, err)
	}
	if parsed.Passed == nil {
		return ConstraintVerdict{}, fmt.Errorf("judge %q: response missing the \"passed\" verdict", j.slug)
	}
	conf, _ := looseFloat(parsed.Confidence)
	return ConstraintVerdict{
		Passed:     *parsed.Passed,
		Constraint: req.Constraint,
		Violations: parsed.Violations,
		Confidence: clamp(conf, 0, 1),
	}, nil
}

func buildConstraintUser(req CheckRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CONSTRAINT:\n%s\n\n", req.Constraint)
	if req.Input != "" {
		fmt.Fprintf(&b, "INPUT:\n%s\n\n", req.Input)
	}
	for i, c := range req.Context {
		fmt.Fprintf(&b, "CONTEXT %d:\n%v\n\n", i+1, c)
	}
	fmt.Fprintf(&b, "OUTPUT:\n%s", req.Output)
	return b.String()
}

// -----------------------------------------------------------------------
// LLM-backed pairwise judge
// -----------------------------------------------------------------------

const pairwiseSystem = `You are a strict comparison judge. Compare OUTPUT A against OUTPUT B on each requested dimension and overall. Respond with ONLY a single JSON object, no prose and no code fences, of exactly this shape:
{"winner": "A" | "B" | "tie", "per_dimension": {"<dimension>": "A" | "B" | "tie", ...}, "margin": <number 0-1>, "explanation": "<one or two sentences>"}`

// LLMPairwiseJudge compares two outputs by prompting a model.
type LLMPairwiseJudge struct {
	slug string
	c    Completer
}

// NewLLMPairwiseJudge builds a pairwise judge backed by the given completer.
func NewLLMPairwiseJudge(slug string, c Completer) *LLMPairwiseJudge {
	return &LLMPairwiseJudge{slug: slug, c: c}
}

func (j *LLMPairwiseJudge) Mode() Mode   { return ModePairwise }
func (j *LLMPairwiseJudge) Slug() string { return j.slug }

// Compare asks the model which output is stronger overall and per dimension.
func (j *LLMPairwiseJudge) Compare(ctx context.Context, req CompareRequest) (PairwiseVerdict, error) {
	raw, err := j.c.Complete(ctx, pairwiseSystem, buildPairwiseUser(req))
	if err != nil {
		return PairwiseVerdict{}, fmt.Errorf("judge %q: complete: %w", j.slug, err)
	}
	var parsed struct {
		Winner       string            `json:"winner"`
		PerDimension map[string]string `json:"per_dimension"`
		Margin       json.RawMessage   `json:"margin"`
		Explanation  string            `json:"explanation"`
	}
	if err := decodeFirstJSON(raw, &parsed); err != nil {
		return PairwiseVerdict{}, fmt.Errorf("judge %q: parse verdict: %w", j.slug, err)
	}
	winner, err := normalizeChoice(parsed.Winner)
	if err != nil {
		return PairwiseVerdict{}, fmt.Errorf("judge %q: %w", j.slug, err)
	}
	perDim := make(map[string]string, len(parsed.PerDimension))
	for d, choice := range parsed.PerDimension {
		if c, err := normalizeChoice(choice); err == nil {
			perDim[d] = c
		}
	}
	margin, _ := looseFloat(parsed.Margin)
	return PairwiseVerdict{
		Winner:       winner,
		PerDimension: perDim,
		Margin:       clamp(margin, 0, 1),
		Explanation:  parsed.Explanation,
	}, nil
}

func buildPairwiseUser(req CompareRequest) string {
	var b strings.Builder
	if len(req.Dimensions) > 0 {
		fmt.Fprintf(&b, "DIMENSIONS: %s\n\n", strings.Join(req.Dimensions, ", "))
	}
	if req.Input != "" {
		fmt.Fprintf(&b, "INPUT:\n%s\n\n", req.Input)
	}
	fmt.Fprintf(&b, "OUTPUT A:\n%s\n\n", req.OutputA)
	fmt.Fprintf(&b, "OUTPUT B:\n%s", req.OutputB)
	return b.String()
}

// normalizeChoice maps a free-form A/B/tie answer to the canonical token.
func normalizeChoice(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "a":
		return "A", nil
	case "b":
		return "B", nil
	case "tie", "draw", "equal":
		return "tie", nil
	default:
		return "", fmt.Errorf("unrecognized choice %q (want A, B, or tie)", s)
	}
}

// -----------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------

// clamp keeps x within [lo, hi].
func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// looseFloat reads a number from raw JSON, accepting both a bare number (8) and
// a quoted number ("8") — a common model quirk. Returns false for anything else.
func looseFloat(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// decodeFirstJSON decodes the first complete JSON object found in raw into v,
// tolerating leading prose or ```json fences (it scans to the first '{') and
// trailing prose (the streaming decoder stops after one complete value). Errors
// when no object is present or the first object is malformed.
func decodeFirstJSON(raw string, v any) error {
	i := strings.IndexByte(raw, '{')
	if i < 0 {
		return fmt.Errorf("no JSON object in response")
	}
	return json.NewDecoder(strings.NewReader(raw[i:])).Decode(v)
}
