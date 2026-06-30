package loom_test

import (
	"context"
	"testing"

	loom "github.com/rhaqim/loom"
)

// jsonGen returns a fixed payload as text, standing in for a judge model.
type jsonGen struct{ payload string }

func (jsonGen) Modality() loom.Modality { return loom.ModalityText }
func (g jsonGen) Generate(context.Context, loom.GenerateRequest) (loom.Result, error) {
	return loom.NewTextResult(g.payload, "stop", 1, 1), nil
}

// imageGen produces a non-text result, to exercise the adapter's type guard.
type imageGen struct{}

func (imageGen) Modality() loom.Modality { return loom.ModalityImage }
func (imageGen) Generate(context.Context, loom.GenerateRequest) (loom.Result, error) {
	return loom.NewImageResult("http://x/y.png", "p", 8, 8), nil
}

// TestLLMRubricJudgeOverGenerator wires the real rubric judge to a text
// generator through the engine's adapter and checks the end-to-end verdict.
func TestLLMRubricJudgeOverGenerator(t *testing.T) {
	gen := jsonGen{payload: `{"scores":{"clarity":9},"explanations":{"clarity":"crisp"}}`}
	j := loom.NewLLMRubricJudge("clarity-judge", gen)

	v, err := j.Score(context.Background(), loom.ScoreRequest{
		Output:     "a sharp sentence",
		Dimensions: []string{"clarity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Scores["clarity"] != 9 || v.Aggregate != 9 {
		t.Fatalf("verdict = %+v", v)
	}
}

// TestGeneratorCompleterRejectsNonText ensures the adapter fails clearly when
// the backing generator is not text-modality.
func TestGeneratorCompleterRejectsNonText(t *testing.T) {
	c := loom.GeneratorCompleter(imageGen{})
	if _, err := c.Complete(context.Background(), "sys", "user"); err == nil {
		t.Fatal("expected an error completing with a non-text generator")
	}
}

// nilGen returns a nil Result interface; typedNilGen returns a typed-nil
// *TextResult. Both must error rather than panic in the adapter.
type nilGen struct{}

func (nilGen) Modality() loom.Modality { return loom.ModalityText }
func (nilGen) Generate(context.Context, loom.GenerateRequest) (loom.Result, error) {
	return nil, nil
}

type typedNilGen struct{}

func (typedNilGen) Modality() loom.Modality { return loom.ModalityText }
func (typedNilGen) Generate(context.Context, loom.GenerateRequest) (loom.Result, error) {
	var tr *loom.TextResult
	return tr, nil
}

func TestGeneratorCompleterHandlesNilResult(t *testing.T) {
	for name, g := range map[string]loom.Generator{"nil-interface": nilGen{}, "typed-nil": typedNilGen{}} {
		c := loom.GeneratorCompleter(g)
		if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
			t.Fatalf("%s: expected an error, not a panic or empty success", name)
		}
	}
}
