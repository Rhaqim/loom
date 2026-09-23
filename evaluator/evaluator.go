// Package evaluator is loom's EXPERIMENTAL decision primitive: a narrow
// interface for asking typed questions about a piece of state and getting back
// calibrated, structured answers that code can branch on.
//
// # Experimental
//
// This package is opt-in and not covered by loom's compatibility promise. It is
// additive — nothing in the engine calls an Evaluator unless the application
// configures one (Config.Evaluator) and wires it into a hook, a flow gate, or a
// judge. With no Evaluator configured every existing code path behaves exactly
// as before. The types here may change shape in a future minor release; pin a
// version if that matters to you.
//
// # Why it exists
//
// A loom Generator PRODUCES content. Everywhere the engine needs to DECIDE
// something about that content it has, until now, had two options: prompt a
// full chat model and parse JSON back (see the judge package), or fall back to
// string heuristics in a hook. Both are expensive or brittle. An Evaluator is
// the missing third option — a classifier that answers many independent
// questions about one state in a single round trip and reports how sure it is.
//
// The three question types are deliberately the closed set of decisions code
// can branch on:
//
//   - Noul   — a yes/no proposition, answered as a probability in 0..1.
//   - Choice — pick one option from a named set, with the full distribution.
//   - Score  — rate the state against an ordered rubric, weighted across levels.
//
// Questions are independent: asking more of them in one call does not make any
// single answer worse, which is what makes fine-grained gating affordable.
//
// # Implementations
//
//   - evaluator/typesafe — TypeSafe's System One API (the Jev model).
//   - evaluator/stub     — deterministic canned answers for tests and offline runs.
//
// The interface is one method, so an application can back it with anything,
// including its own classifier.
package evaluator

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// Type identifies the shape of a Question and of the Answer it produces.
type Type string

const (
	// TypeNoul is a yes/no question answered as a probability in 0..1.
	TypeNoul Type = "noul"
	// TypeChoice picks one option from a named set.
	TypeChoice Type = "choice"
	// TypeScore rates the state against an ordered rubric of levels.
	TypeScore Type = "score"
)

// Question is one typed question about the evaluated state.
//
// Instructions is the question itself. It is usually a string, but may be a
// map or slice when the question needs to carry reference data alongside the
// prompt — put the question in one field and the data in others, and refer to
// the data fields by name.
//
// Criteria is type-dependent and is best set through the Noul, Choice and
// Score constructors rather than by hand:
//
//   - Noul   — optional; a map with "true" and "false" keys describing each side.
//   - Choice — required; a map of option name to its description.
//   - Score  — required; an ordered slice of level descriptions, low to high.
type Question struct {
	Type         Type
	Instructions any
	Criteria     any
}

// Noul builds a yes/no question. Use NoulWith to describe what each side means.
func Noul(instructions any) Question {
	return Question{Type: TypeNoul, Instructions: instructions}
}

// NoulWith builds a yes/no question that spells out what a yes and a no mean.
// Describing both sides is the cheapest way to sharpen a vague proposition.
func NoulWith(instructions, ifTrue, ifFalse any) Question {
	return Question{
		Type:         TypeNoul,
		Instructions: instructions,
		Criteria:     map[string]any{"true": ifTrue, "false": ifFalse},
	}
}

// Choice builds a question that picks one option from options, a map of option
// name to a description of when that option applies. A nil description is
// allowed for an option that needs no elaboration.
func Choice(instructions any, options map[string]any) Question {
	return Question{Type: TypeChoice, Instructions: instructions, Criteria: options}
}

// ChoiceOf builds a Choice over bare option names with no descriptions. Prefer
// Choice with descriptions whenever the options could be confused for one
// another — the descriptions are what separate them.
func ChoiceOf(instructions any, options ...string) Question {
	crit := make(map[string]any, len(options))
	for _, o := range options {
		crit[o] = nil
	}
	return Question{Type: TypeChoice, Instructions: instructions, Criteria: crit}
}

// Score builds a rubric question over ordered levels, lowest first. Levels
// should describe concrete situations rather than abstract degrees: "broken,
// but a workaround exists" separates cleanly, "moderately severe" does not.
func Score(instructions any, levels ...string) Question {
	lv := make([]any, len(levels))
	for i, l := range levels {
		lv[i] = l
	}
	return Question{Type: TypeScore, Instructions: instructions, Criteria: lv}
}

// ScoreOf builds a rubric question whose levels are structured values rather
// than plain strings, for levels that need their own fields (a description plus
// examples, say).
func ScoreOf(instructions any, levels []any) Question {
	return Question{Type: TypeScore, Instructions: instructions, Criteria: levels}
}

// Validate reports whether the question is well formed. Implementations call it
// before dispatching so a malformed question fails locally rather than as a
// provider-side validation error.
func (q Question) Validate() error {
	switch q.Type {
	case TypeNoul:
		// Criteria is optional; when present it must be a map of side → meaning.
		if q.Criteria != nil {
			if _, ok := q.Criteria.(map[string]any); !ok {
				return fmt.Errorf("noul criteria must be a map with \"true\"/\"false\" keys, got %T", q.Criteria)
			}
		}
	case TypeChoice:
		opts, ok := q.Criteria.(map[string]any)
		if !ok {
			return fmt.Errorf("choice criteria must be a map of option → description, got %T", q.Criteria)
		}
		if len(opts) < 2 {
			return fmt.Errorf("choice needs at least 2 options, got %d", len(opts))
		}
	case TypeScore:
		levels, ok := q.Criteria.([]any)
		if !ok {
			return fmt.Errorf("score criteria must be an ordered slice of levels, got %T", q.Criteria)
		}
		if len(levels) < 2 {
			return fmt.Errorf("score needs at least 2 levels, got %d", len(levels))
		}
	case "":
		return fmt.Errorf("question has no type")
	default:
		return fmt.Errorf("unknown question type %q", q.Type)
	}
	if q.Instructions == nil {
		return fmt.Errorf("question has no instructions")
	}
	if s, ok := q.Instructions.(string); ok && s == "" {
		return fmt.Errorf("question has empty instructions")
	}
	return nil
}

// Answer is the result of one Question. Which fields carry meaning depends on
// Type; the accessors below read the right one for you.
type Answer struct {
	// Type mirrors the question's type.
	Type Type
	// Noul is the probability the answer is yes, 0..1. Set for TypeNoul.
	Noul float64
	// Choice is the highest-probability option. Set for TypeChoice.
	Choice string
	// Score is the probability-weighted position across the rubric levels, and
	// so can land between them. Set for TypeScore.
	Score float64
	// Legend maps each level index (as a string) back to its description. Set
	// for TypeScore.
	Legend map[string]string
	// Probabilities maps each option (Choice) or level index (Score) to its
	// probability. The values sum to 1.
	Probabilities map[string]float64
	// Confidence, 0..1, is how concentrated the distribution is on one outcome.
	// Set for TypeChoice and TypeScore; for TypeNoul use Certainty, which
	// derives the equivalent from the probability itself.
	Confidence float64
}

// Yes reports whether a Noul answer clears threshold. Choose the threshold from
// the cost of being wrong: a high one (0.8+) when a false positive is expensive,
// a low one (0.2) when a false negative is, and route the middle to a human or
// to a more expensive check.
func (a Answer) Yes(threshold float64) bool { return a.Noul >= threshold }

// No reports whether a Noul answer is at or below threshold.
func (a Answer) No(threshold float64) bool { return a.Noul <= threshold }

// Is reports whether a Choice answer selected option.
func (a Answer) Is(option string) bool { return a.Choice == option }

// P returns the probability assigned to an option (Choice) or level index
// (Score), or 0 if it is absent.
func (a Answer) P(key string) float64 { return a.Probabilities[key] }

// Level rounds a Score answer to the nearest whole rubric level. Use Score
// itself when the fractional position matters — a 1.5 on a three-level rubric
// means the model genuinely split between two levels, which is information
// rounding throws away.
func (a Answer) Level() int { return int(math.Round(a.Score)) }

// Certainty is how sure the model is, 0..1, across all three types. For Choice
// and Score it is Confidence; for Noul it is derived from the probability
// itself, since a Noul at 0.5 is maximally uncertain and one at 0 or 1 is
// maximally certain.
func (a Answer) Certainty() float64 {
	if a.Type == TypeNoul {
		return math.Abs(a.Noul-0.5) * 2
	}
	return a.Confidence
}

// Certain reports whether Certainty is at or above min. It is the gate for
// confidence-routed code: act on the answer when it is certain, and fall back
// to a slower, more expensive path when it is not.
func (a Answer) Certain(min float64) bool { return a.Certainty() >= min }

// Ranked returns the outcomes ordered by descending probability, ties broken by
// key so the order is deterministic. Useful for keeping the top-K candidates
// instead of committing to the single best one.
func (a Answer) Ranked() []Outcome {
	out := make([]Outcome, 0, len(a.Probabilities))
	for k, p := range a.Probabilities {
		out = append(out, Outcome{Key: k, P: p})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].P != out[j].P {
			return out[i].P > out[j].P
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Outcome is one option (Choice) or level (Score) with its probability.
type Outcome struct {
	Key string
	P   float64
}

// Answers maps each question id to its Answer. The accessors are nil- and
// miss-safe: a question that was never asked, or whose answer did not come
// back, reads as a zero Answer rather than panicking — so a gate written
// against a missing question fails open rather than crashing the turn.
type Answers map[string]Answer

// Get returns the answer for id, or the zero Answer if absent.
func (a Answers) Get(id string) Answer { return a[id] }

// Has reports whether an answer came back for id.
func (a Answers) Has(id string) bool { _, ok := a[id]; return ok }

// Yes reports whether the Noul answer for id clears threshold. A missing answer
// is false.
func (a Answers) Yes(id string, threshold float64) bool { return a[id].Yes(threshold) }

// Choice returns the selected option for id, or "" if absent.
func (a Answers) Choice(id string) string { return a[id].Choice }

// Score returns the weighted score for id, or 0 if absent.
func (a Answers) Score(id string) float64 { return a[id].Score }

// Noul returns the yes-probability for id, or 0 if absent.
func (a Answers) Noul(id string) float64 { return a[id].Noul }

// Certainty returns how sure the model is about id, or 0 if absent.
func (a Answers) Certainty(id string) float64 { return a[id].Certainty() }

// Usage reports the tokens an evaluation consumed, for cost accounting.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Evaluation is the result of one Evaluate call.
type Evaluation struct {
	// Model is the concrete model that answered, which may be more specific
	// than the alias that was requested.
	Model string
	// Answers holds one entry per question, under the ids that were asked.
	Answers Answers
	// Usage is the token cost of the call.
	Usage Usage
}

// Evaluator answers typed questions about a state.
//
// Implementations must evaluate every question independently — the answer to
// one must not depend on the others being present — and must return an answer
// for every question asked, or an error. A partial result is an error.
//
// state is the content the questions are about. A string for plain text, or any
// JSON-encodable value for structured state: a session's variables, a record, a
// chat log. Passing the whole state and asking narrow questions about it is the
// intended shape, not an abuse of it.
//
// Implementations must be safe for concurrent use.
type Evaluator interface {
	Evaluate(ctx context.Context, state any, questions map[string]Question) (Evaluation, error)
}

// ValidateQuestions checks a whole question set, returning the first problem it
// finds with the offending id named. Implementations call it before dispatch.
func ValidateQuestions(questions map[string]Question) error {
	if len(questions) == 0 {
		return fmt.Errorf("no questions to evaluate")
	}
	// Iterate in sorted order so the reported error is deterministic when more
	// than one question is malformed.
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if id == "" {
			return fmt.Errorf("question id must not be empty")
		}
		if err := questions[id].Validate(); err != nil {
			return fmt.Errorf("question %q: %w", id, err)
		}
	}
	return nil
}
