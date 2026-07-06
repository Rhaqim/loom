package judge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCompleter returns a canned response and records what it was asked.
type fakeCompleter struct {
	resp               string
	err                error
	gotSystem, gotUser string
}

func (f *fakeCompleter) Complete(_ context.Context, system, user string) (string, error) {
	f.gotSystem, f.gotUser = system, user
	return f.resp, f.err
}

func TestLLMRubricJudgeScores(t *testing.T) {
	fc := &fakeCompleter{resp: `{"scores":{"coherence":8,"novelty":6},"explanations":{"coherence":"flows well","novelty":"familiar"}}`}
	j := NewLLMRubricJudge("r", fc)

	v, err := j.Score(context.Background(), ScoreRequest{
		Input:      "write a line",
		Output:     "the line",
		Dimensions: []string{"coherence", "novelty"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Scores["coherence"] != 8 || v.Scores["novelty"] != 6 {
		t.Fatalf("scores = %+v", v.Scores)
	}
	if v.Aggregate != 7 {
		t.Fatalf("aggregate = %v, want 7", v.Aggregate)
	}
	if v.Explanations["coherence"] != "flows well" {
		t.Fatalf("explanation = %q", v.Explanations["coherence"])
	}
	// the output to score must reach the model
	if !strings.Contains(fc.gotUser, "the line") {
		t.Fatalf("user prompt missing output: %q", fc.gotUser)
	}
}

func TestLLMRubricJudgeClampsAndFillsMissing(t *testing.T) {
	// novelty is over-range and clamped; faithfulness is omitted and defaults.
	fc := &fakeCompleter{resp: `{"scores":{"coherence":7,"novelty":42}}`}
	j := NewLLMRubricJudge("r", fc)

	v, err := j.Score(context.Background(), ScoreRequest{
		Output:     "x",
		Dimensions: []string{"coherence", "novelty", "faithfulness"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Scores["novelty"] != 10 {
		t.Fatalf("novelty not clamped: %v", v.Scores["novelty"])
	}
	if v.Scores["faithfulness"] != 5.0 {
		t.Fatalf("missing dimension not defaulted: %v", v.Scores["faithfulness"])
	}
	want := (7.0 + 10.0 + 5.0) / 3.0
	if v.Aggregate != want {
		t.Fatalf("aggregate = %v, want %v", v.Aggregate, want)
	}
}

func TestLLMRubricJudgeToleratesFencedJSON(t *testing.T) {
	fc := &fakeCompleter{resp: "Sure!\n```json\n{\"scores\":{\"q\":9}}\n```\n"}
	j := NewLLMRubricJudge("r", fc)
	v, err := j.Score(context.Background(), ScoreRequest{Output: "x", Dimensions: []string{"q"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Scores["q"] != 9 {
		t.Fatalf("score = %v, want 9 (fenced JSON not extracted)", v.Scores["q"])
	}
}

func TestLLMRubricJudgeErrors(t *testing.T) {
	j := NewLLMRubricJudge("r", &fakeCompleter{err: errors.New("boom")})
	if _, err := j.Score(context.Background(), ScoreRequest{Output: "x", Dimensions: []string{"q"}}); err == nil {
		t.Fatal("expected completer error to propagate")
	}
	j = NewLLMRubricJudge("r", &fakeCompleter{resp: "not json"})
	if _, err := j.Score(context.Background(), ScoreRequest{Output: "x", Dimensions: []string{"q"}}); err == nil {
		t.Fatal("expected parse error on non-JSON reply")
	}
	if _, err := j.Score(context.Background(), ScoreRequest{Output: "x"}); err == nil {
		t.Fatal("expected error when no dimensions are requested")
	}
}

func TestLLMConstraintJudge(t *testing.T) {
	fc := &fakeCompleter{resp: `{"passed":false,"violations":["contradicts the established fact"],"confidence":1.7}`}
	j := NewLLMConstraintJudge("c", fc)

	v, err := j.Check(context.Background(), CheckRequest{
		Output:     "the sky is green",
		Constraint: "must not contradict known facts",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Passed {
		t.Fatal("expected failed verdict")
	}
	if len(v.Violations) != 1 {
		t.Fatalf("violations = %v", v.Violations)
	}
	if v.Confidence != 1.0 {
		t.Fatalf("confidence not clamped: %v", v.Confidence)
	}
	if v.Constraint != "must not contradict known facts" {
		t.Fatalf("constraint not echoed: %q", v.Constraint)
	}
}

func TestLLMConstraintJudgeErrors(t *testing.T) {
	j := NewLLMConstraintJudge("c", &fakeCompleter{resp: `{}`})
	if _, err := j.Check(context.Background(), CheckRequest{Output: "x", Constraint: ""}); err == nil {
		t.Fatal("expected error on empty constraint")
	}
}

func TestLLMPairwiseJudge(t *testing.T) {
	fc := &fakeCompleter{resp: `{"winner":"a","per_dimension":{"style":"B","logic":"tie"},"margin":1.5,"explanation":"A is tighter"}`}
	j := NewLLMPairwiseJudge("p", fc)

	v, err := j.Compare(context.Background(), CompareRequest{
		OutputA:    "alpha",
		OutputB:    "beta",
		Dimensions: []string{"style", "logic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Winner != "A" {
		t.Fatalf("winner = %q, want normalized A", v.Winner)
	}
	if v.PerDimension["style"] != "B" || v.PerDimension["logic"] != "tie" {
		t.Fatalf("per-dimension = %v", v.PerDimension)
	}
	if v.Margin != 1.0 {
		t.Fatalf("margin not clamped: %v", v.Margin)
	}
}

func TestLLMPairwiseJudgeRejectsBadWinner(t *testing.T) {
	j := NewLLMPairwiseJudge("p", &fakeCompleter{resp: `{"winner":"maybe"}`})
	if _, err := j.Compare(context.Background(), CompareRequest{OutputA: "a", OutputB: "b"}); err == nil {
		t.Fatal("expected error on unrecognized winner")
	}
}

func TestLLMRubricJudgeEmptyScoresErrors(t *testing.T) {
	// Valid JSON but no requested-dimension scores must error, not pass as neutral.
	for _, resp := range []string{`{}`, `{"scores":null}`, `{"verdict":{"scores":{"q":8}}}`} {
		j := NewLLMRubricJudge("r", &fakeCompleter{resp: resp})
		if _, err := j.Score(context.Background(), ScoreRequest{Output: "x", Dimensions: []string{"q"}}); err == nil {
			t.Fatalf("expected error for wrong-shaped reply %q", resp)
		}
	}
}

func TestLLMRubricJudgeToleratesQuotedNumbers(t *testing.T) {
	j := NewLLMRubricJudge("r", &fakeCompleter{resp: `{"scores":{"q":"8"}}`})
	v, err := j.Score(context.Background(), ScoreRequest{Output: "x", Dimensions: []string{"q"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Scores["q"] != 8 {
		t.Fatalf("quoted number not parsed: %v", v.Scores["q"])
	}
}

func TestLLMConstraintJudgeMissingPassedErrors(t *testing.T) {
	// A reply lacking the passed verdict must error, not synthesize a failure.
	j := NewLLMConstraintJudge("c", &fakeCompleter{resp: `{"violations":[]}`})
	if _, err := j.Check(context.Background(), CheckRequest{Output: "x", Constraint: "rule"}); err == nil {
		t.Fatal("expected error when the reply omits passed")
	}
}

func TestLLMConstraintJudgeRendersContext(t *testing.T) {
	fc := &fakeCompleter{resp: `{"passed":true,"violations":[],"confidence":0.8}`}
	j := NewLLMConstraintJudge("c", fc)
	if _, err := j.Check(context.Background(), CheckRequest{
		Output:     "x",
		Constraint: "must not contradict the facts",
		Context:    []any{"fact: the sky is blue"},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.gotUser, "the sky is blue") {
		t.Fatalf("context not rendered into the prompt: %q", fc.gotUser)
	}
}

func TestRegistryRegisterAsHonorsSlug(t *testing.T) {
	reg := NewRegistry()
	j := NewLLMRubricJudge("intrinsic", &fakeCompleter{resp: `{"scores":{"q":9}}`})
	reg.RegisterAs("explicit", j)

	if reg.Rubric("explicit") != j {
		t.Fatal("lookup by the explicit slug did not return the registered judge")
	}
	// the intrinsic slug was never registered, so it falls back to the no-op
	v, _ := reg.Rubric("intrinsic").Score(context.Background(), ScoreRequest{Output: "x", Dimensions: []string{"q"}})
	if v.Aggregate != 5.0 {
		t.Fatalf("expected neutral no-op for unregistered slug, got %v", v.Aggregate)
	}
}

func TestNoopConstraintDoesNotFailOpenWithCertainty(t *testing.T) {
	reg := NewRegistry()
	v, _ := reg.Constraint("missing").Check(context.Background(), CheckRequest{Output: "x", Constraint: "r"})
	if v.Confidence != 0 {
		t.Fatalf("no-op constraint asserted confidence %v, want 0", v.Confidence)
	}
	if len(v.Violations) == 0 {
		t.Fatal("no-op constraint should flag that the check did not run")
	}
}

func TestDecodeFirstJSON(t *testing.T) {
	// Trailing prose containing a brace must not break decoding.
	var got struct {
		Passed bool `json:"passed"`
	}
	in := "{\"passed\":true}\n\nNote: used the {passed, ...} format."
	if err := decodeFirstJSON(in, &got); err != nil {
		t.Fatalf("decode failed on trailing prose: %v", err)
	}
	if !got.Passed {
		t.Fatal("decoded the wrong value")
	}
	// No object present is an error.
	if err := decodeFirstJSON("no json here", &got); err == nil {
		t.Fatal("expected error when no object present")
	}
}
