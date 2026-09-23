package engine

// api_evaluator.go is the engine-side surface of the EXPERIMENTAL evaluator
// subsystem: the decision counterpart to Generator.
//
// A Generator PRODUCES content — its Result is persisted, streamed, forked and
// costed as part of the session. An Evaluator DECIDES things about content: it
// answers typed questions (yes/no, pick-one, rate-against-a-rubric) as
// calibrated probabilities that code branches on. It produces no Result, opens
// no step, and writes nothing to the session, which is why it is a separate
// interface rather than another Generator flavour.
//
// The whole subsystem is opt-in and additive. With Config.Evaluator unset,
// Evaluate returns ErrEvaluatorNotConfigured and every other engine code path
// is byte-for-byte what it was before — no hook is registered, no flow gate is
// consulted, no judge changes behaviour. Nothing in the engine calls an
// Evaluator on its own; the application wires it into a hook (see EvalGate), a
// flow gate (FlowAgent.When), or a judge (evaluator.NewRubricJudge and friends).
//
// EXPERIMENTAL: this surface is not covered by loom's compatibility promise and
// may change shape in a future minor release.

import (
	"context"
	"fmt"

	"github.com/rhaqim/loom/evaluator"
)

// Evaluator answers typed questions about a state, returning calibrated
// answers rather than prose. See the evaluator package for the full contract
// and for the typesafe (TypeSafe System One / Jev) and stub implementations.
//
// EXPERIMENTAL.
type Evaluator = evaluator.Evaluator

// EvalQuestion is one typed question about an evaluated state. Build them with
// EvalNoul, EvalChoice and EvalScore rather than by hand.
//
// EXPERIMENTAL.
type EvalQuestion = evaluator.Question

// EvalAnswer is the result of one EvalQuestion. Which fields carry meaning
// depends on its type; the accessors (Yes, Is, Level, Certain) read the right
// one.
//
// EXPERIMENTAL.
type EvalAnswer = evaluator.Answer

// EvalAnswers maps each question id to its answer. Its accessors are miss-safe,
// so a gate written against a question that was never answered fails open
// rather than panicking mid-turn.
//
// EXPERIMENTAL.
type EvalAnswers = evaluator.Answers

// Evaluation is the result of one Evaluate call: the answers, the model that
// produced them, and the token usage.
//
// EXPERIMENTAL.
type Evaluation = evaluator.Evaluation

// EvalUsage reports the tokens an evaluation consumed.
//
// EXPERIMENTAL.
type EvalUsage = evaluator.Usage

// EvalOutcome is one option or rubric level with its probability, as returned
// by EvalAnswer.Ranked.
//
// EXPERIMENTAL.
type EvalOutcome = evaluator.Outcome

// EvalType identifies the shape of an EvalQuestion.
//
// EXPERIMENTAL.
type EvalType = evaluator.Type

// EvalNoulType is a yes/no question answered as a probability in 0..1.
const EvalNoulType = evaluator.TypeNoul

// EvalChoiceType picks one option from a named set.
const EvalChoiceType = evaluator.TypeChoice

// EvalScoreType rates the state against an ordered rubric of levels.
const EvalScoreType = evaluator.TypeScore

// EvalNoul builds a yes/no question. EXPERIMENTAL.
var EvalNoul = evaluator.Noul

// EvalNoulWith builds a yes/no question that spells out what a yes and a no
// mean. Describing both sides is the cheapest way to sharpen a vague
// proposition. EXPERIMENTAL.
var EvalNoulWith = evaluator.NoulWith

// EvalChoice builds a question that picks one option from a map of option name
// to description. EXPERIMENTAL.
var EvalChoice = evaluator.Choice

// EvalChoiceOf builds a Choice over bare option names with no descriptions.
// EXPERIMENTAL.
var EvalChoiceOf = evaluator.ChoiceOf

// EvalScore builds a rubric question over ordered levels, lowest first.
// EXPERIMENTAL.
var EvalScore = evaluator.Score

// EvalScoreOf builds a rubric question whose levels are structured values
// rather than plain strings. EXPERIMENTAL.
var EvalScoreOf = evaluator.ScoreOf

// Evaluator returns the configured Evaluator, or nil when none is set. Use it
// to check whether the experimental subsystem is available before registering a
// gate that depends on it.
//
// EXPERIMENTAL.
func (e *Engine) Evaluator() Evaluator {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.evaluator
}

// SetEvaluator installs or replaces the engine's Evaluator at runtime, mirroring
// RegisterGenerator. Pass nil to remove it, which returns the engine to its
// unconfigured behaviour.
//
// EXPERIMENTAL.
func (e *Engine) SetEvaluator(ev Evaluator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evaluator = ev
}

// Evaluate asks the configured Evaluator a set of typed questions about state,
// returning one answer per question.
//
// The questions are evaluated independently, so ask everything the decision
// needs in ONE call rather than several: that is the property the subsystem
// exists for, and splitting a decision across calls throws it away.
//
// It returns ErrEvaluatorNotConfigured when no Evaluator is configured, so a
// caller can distinguish "the feature is off" from "the evaluation failed" and
// fall back accordingly.
//
// EXPERIMENTAL.
func (e *Engine) Evaluate(ctx context.Context, state any, questions map[string]EvalQuestion) (Evaluation, error) {
	ev := e.Evaluator()
	if ev == nil {
		return Evaluation{}, ErrEvaluatorNotConfigured
	}
	eval, err := ev.Evaluate(ctx, state, questions)
	if err != nil {
		return Evaluation{}, fmt.Errorf("loom: evaluate: %w", err)
	}
	return eval, nil
}
