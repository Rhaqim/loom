// Package stub provides a deterministic, offline evaluator.Evaluator for tests
// and for running an application with no provider credentials — the counterpart
// to generator/echo on the EXPERIMENTAL evaluator side.
//
// It never makes a network call and always answers every question asked, so a
// pipeline gated on evaluations still runs end to end offline. Canned answers
// are set per question id; anything not set falls back to a neutral default
// that deliberately fires no gate:
//
//	ev := stub.New().
//	    WithNoul("has_cliches", 0.05).
//	    WithScore("quality", 2.6).
//	    WithChoice("intent", "explore")
package stub

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/rhaqim/loom/evaluator"
)

// Call records one Evaluate invocation, for assertions in tests.
type Call struct {
	State     any
	Questions map[string]evaluator.Question
}

// Evaluator answers from a canned table, falling back to neutral defaults.
// It is safe for concurrent use.
type Evaluator struct {
	mu sync.Mutex

	answers map[string]evaluator.Answer
	err     error
	calls   []Call

	// defaultNoul is the yes-probability returned for an unconfigured Noul.
	// It defaults to 0 — a firm "no" — because the common use of a Noul in a
	// gate is "is something wrong with this?", and a stub should let the
	// pipeline through rather than trip every gate offline.
	defaultNoul float64
}

// New creates a stub evaluator with no canned answers.
func New() *Evaluator {
	return &Evaluator{answers: map[string]evaluator.Answer{}}
}

// WithAnswer sets the full Answer returned for a question id, for cases the
// typed helpers below do not cover (a specific probability distribution, say).
func (e *Evaluator) WithAnswer(id string, a evaluator.Answer) *Evaluator {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.answers[id] = a
	return e
}

// WithNoul sets the yes-probability returned for a Noul question id.
func (e *Evaluator) WithNoul(id string, p float64) *Evaluator {
	return e.WithAnswer(id, evaluator.Answer{Type: evaluator.TypeNoul, Noul: p})
}

// WithChoice sets the option returned for a Choice question id. The probability
// distribution puts 1.0 on that option, so the answer reads as fully certain.
func (e *Evaluator) WithChoice(id, option string) *Evaluator {
	return e.WithAnswer(id, evaluator.Answer{
		Type:          evaluator.TypeChoice,
		Choice:        option,
		Probabilities: map[string]float64{option: 1},
		Confidence:    1,
	})
}

// WithScore sets the weighted score returned for a Score question id.
func (e *Evaluator) WithScore(id string, score float64) *Evaluator {
	return e.WithAnswer(id, evaluator.Answer{
		Type:       evaluator.TypeScore,
		Score:      score,
		Confidence: 1,
	})
}

// WithDefaultNoul changes the yes-probability returned for Noul questions that
// have no canned answer.
func (e *Evaluator) WithDefaultNoul(p float64) *Evaluator {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.defaultNoul = p
	return e
}

// WithError makes every Evaluate call fail with err, for exercising the
// caller's error path.
func (e *Evaluator) WithError(err error) *Evaluator {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
	return e
}

// Calls returns a copy of the recorded invocations, oldest first.
func (e *Evaluator) Calls() []Call {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Call(nil), e.calls...)
}

// Reset clears the recorded invocations, leaving canned answers in place.
func (e *Evaluator) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = nil
}

// Evaluate returns the canned answer for each question id, or a neutral default
// derived from the question itself. Questions are validated exactly as a real
// evaluator validates them, so a malformed question fails the same way offline
// as it would against the provider.
func (e *Evaluator) Evaluate(_ context.Context, state any, questions map[string]evaluator.Question) (evaluator.Evaluation, error) {
	if err := evaluator.ValidateQuestions(questions); err != nil {
		return evaluator.Evaluation{}, fmt.Errorf("stub: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return evaluator.Evaluation{}, e.err
	}
	e.calls = append(e.calls, Call{State: state, Questions: questions})

	answers := make(evaluator.Answers, len(questions))
	for id, q := range questions {
		if a, ok := e.answers[id]; ok {
			answers[id] = fill(a, q)
			continue
		}
		answers[id] = neutral(q, e.defaultNoul)
	}
	return evaluator.Evaluation{Model: "stub", Answers: answers}, nil
}

// fill completes a canned answer from its question, so a caller who set only a
// score or a choice still gets a coherent answer: the right type, a legend for
// a Score, and a distribution consistent with the value.
func fill(a evaluator.Answer, q evaluator.Question) evaluator.Answer {
	a.Type = q.Type
	switch q.Type {
	case evaluator.TypeScore:
		if a.Legend == nil {
			a.Legend = legendOf(q)
		}
		if a.Probabilities == nil {
			a.Probabilities = map[string]float64{fmt.Sprintf("%d", a.Level()): 1}
		}
	case evaluator.TypeChoice:
		if a.Probabilities == nil && a.Choice != "" {
			a.Probabilities = map[string]float64{a.Choice: 1}
		}
	}
	return a
}

// neutral is the answer for a question with nothing canned: a Noul at the
// configured default, a Choice on the first option in sorted order, and a Score
// at the middle of the rubric. Every default is deterministic, so a test that
// does not configure an answer still gets the same result on every run.
func neutral(q evaluator.Question, defaultNoul float64) evaluator.Answer {
	switch q.Type {
	case evaluator.TypeNoul:
		return evaluator.Answer{Type: evaluator.TypeNoul, Noul: defaultNoul}

	case evaluator.TypeChoice:
		opts := optionsOf(q)
		a := evaluator.Answer{Type: evaluator.TypeChoice, Probabilities: map[string]float64{}}
		if len(opts) == 0 {
			return a
		}
		// Spread the distribution evenly and name the first option as the
		// choice: an even spread is the honest shape for "no opinion", and it
		// keeps Certain() false so confidence-gated code takes its fallback
		// path offline instead of acting on a fabricated certainty.
		p := 1 / float64(len(opts))
		for _, o := range opts {
			a.Probabilities[o] = p
		}
		a.Choice = opts[0]
		a.Confidence = p
		return a

	case evaluator.TypeScore:
		levels, _ := q.Criteria.([]any)
		a := evaluator.Answer{Type: evaluator.TypeScore, Legend: legendOf(q)}
		if len(levels) == 0 {
			return a
		}
		a.Score = float64(len(levels)-1) / 2
		p := 1 / float64(len(levels))
		a.Probabilities = make(map[string]float64, len(levels))
		for i := range levels {
			a.Probabilities[fmt.Sprintf("%d", i)] = p
		}
		a.Confidence = p
		return a
	}
	return evaluator.Answer{Type: q.Type}
}

// optionsOf returns a Choice question's options in sorted order, so the stub's
// pick does not depend on Go's randomised map iteration.
func optionsOf(q evaluator.Question) []string {
	crit, _ := q.Criteria.(map[string]any)
	opts := make([]string, 0, len(crit))
	for o := range crit {
		opts = append(opts, o)
	}
	sort.Strings(opts)
	return opts
}

// legendOf echoes a Score question's levels back as the answer legend, matching
// what a real provider returns.
func legendOf(q evaluator.Question) map[string]string {
	levels, _ := q.Criteria.([]any)
	if len(levels) == 0 {
		return nil
	}
	legend := make(map[string]string, len(levels))
	for i, l := range levels {
		if s, ok := l.(string); ok {
			legend[fmt.Sprintf("%d", i)] = s
			continue
		}
		legend[fmt.Sprintf("%d", i)] = fmt.Sprintf("%v", l)
	}
	return legend
}
