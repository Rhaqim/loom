package loom

// judges.go wires the judge sub-package into the engine's JudgeRegistry
// interface so callers use a single coherent API surface.

import (
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
	j.reg.Register(jj)
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
