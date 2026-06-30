package loom

// judges.go wires the judge sub-package into the engine's JudgeRegistry
// interface so callers use a single coherent API surface.

import (
	"context"
	"fmt"

	"github.com/rhaqim/loom/judge"
)

// judgeRegistryImpl wraps judge.Registry to satisfy the JudgeRegistry interface
// declared in engine.go.
type judgeRegistryImpl struct {
	reg *judge.Registry
}

func newJudgeRegistryImpl() *judgeRegistryImpl {
	return &judgeRegistryImpl{reg: judge.NewRegistry()}
}

func (j *judgeRegistryImpl) Rubric(slug string) RubricJudge {
	return j.reg.Rubric(slug)
}

func (j *judgeRegistryImpl) Pairwise(slug string) PairwiseJudge {
	return j.reg.Pairwise(slug)
}

func (j *judgeRegistryImpl) Constraint(slug string) ConstraintJudge {
	return j.reg.Constraint(slug)
}

func (j *judgeRegistryImpl) Register(slug string, jj Judge) {
	j.reg.RegisterAs(slug, jj)
}

// Expose judge sub-package types as aliases so callers don't need a separate import.

// RubricJudge scores a single output on multiple dimensions.
type RubricJudge = judge.RubricJudge

// PairwiseJudge compares two outputs and picks the stronger one.
type PairwiseJudge = judge.PairwiseJudge

// ConstraintJudge checks whether an output satisfies a stated rule.
type ConstraintJudge = judge.ConstraintJudge

// Judge is the base interface all judges implement.
type Judge = judge.Judge

// ScoreRequest is the input to RubricJudge.Score.
type ScoreRequest = judge.ScoreRequest

// RubricVerdict is the output of RubricJudge.Score.
type RubricVerdict = judge.RubricVerdict

// CompareRequest is the input to PairwiseJudge.Compare.
type CompareRequest = judge.CompareRequest

// PairwiseVerdict is the output of PairwiseJudge.Compare.
type PairwiseVerdict = judge.PairwiseVerdict

// CheckRequest is the input to ConstraintJudge.Check.
type CheckRequest = judge.CheckRequest

// ConstraintVerdict is the output of ConstraintJudge.Check.
type ConstraintVerdict = judge.ConstraintVerdict

// Completer is the narrow text-generation capability the LLM-backed judges need.
type Completer = judge.Completer

// NewLLMRubricJudge builds a rubric judge backed by a text generator.
func NewLLMRubricJudge(slug string, g Generator) *judge.LLMRubricJudge {
	return judge.NewLLMRubricJudge(slug, GeneratorCompleter(g))
}

// NewLLMConstraintJudge builds a constraint judge backed by a text generator.
func NewLLMConstraintJudge(slug string, g Generator) *judge.LLMConstraintJudge {
	return judge.NewLLMConstraintJudge(slug, GeneratorCompleter(g))
}

// NewLLMPairwiseJudge builds a pairwise judge backed by a text generator.
func NewLLMPairwiseJudge(slug string, g Generator) *judge.LLMPairwiseJudge {
	return judge.NewLLMPairwiseJudge(slug, GeneratorCompleter(g))
}

// GeneratorCompleter adapts a text Generator to judge.Completer so engine
// generators can back real judges. The generator must return a TextResult.
func GeneratorCompleter(g Generator) judge.Completer {
	return generatorCompleter{g: g}
}

type generatorCompleter struct{ g Generator }

func (a generatorCompleter) Complete(ctx context.Context, system, user string) (string, error) {
	res, err := a.g.Generate(ctx, GenerateRequest{SystemPrompt: system, UserPrompt: user})
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", fmt.Errorf("judge completer: generator returned no result")
	}
	tr, ok := res.(*TextResult)
	if !ok {
		return "", fmt.Errorf("judge completer: generator produced %s, want text", res.Modality())
	}
	if tr == nil {
		return "", fmt.Errorf("judge completer: nil text result")
	}
	return tr.Content, nil
}
