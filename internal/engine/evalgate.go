package engine

// evalgate.go turns evaluation answers into control flow: a post-hook that
// accepts a result, sends it back for a retry with a reason, or fails the step.
//
// It is the bridge between the EXPERIMENTAL evaluator subsystem and loom's
// existing retry machinery. Nothing here runs unless the application both sets
// Config.Evaluator and registers the returned hook, so the gate is opt-in twice
// over; an engine with neither behaves exactly as it did before.
//
// The gate exists because the post-hook contract is binary — return the result,
// or return ErrRetryWith — while a calibrated answer is not. A quality check
// that comes back at 0.52 is not a pass and not a failure; it is a case for a
// third branch. EvalGateConfig.Decide is where the application draws those
// lines, in code, from numbers it can log and tune.

import (
	"context"
	"fmt"
)

// EvalVerdict is what a gate decides about a result.
//
// EXPERIMENTAL.
type EvalVerdict int

const (
	// EvalAccept passes the result through unchanged. The zero value, so a
	// Decide that falls off the end accepts rather than blocking the turn.
	EvalAccept EvalVerdict = iota
	// EvalRetry sends the result back for another attempt, carrying the
	// decision's Reason and Forbidden list to the generator as a retry
	// annotation — the same path a hand-written validation hook uses.
	EvalRetry
	// EvalReject fails the step outright with the decision's Reason. Use it for
	// a violation no retry will fix (a safety refusal, a hard policy breach);
	// prefer EvalRetry for quality problems, which usually do improve.
	EvalReject
)

// EvalDecision is the outcome of a gate policy.
//
// EXPERIMENTAL.
type EvalDecision struct {
	// Verdict is what to do with the result.
	Verdict EvalVerdict
	// Reason explains the verdict. On EvalRetry it becomes the retry
	// annotation the next attempt sees, so write it as an instruction to the
	// model ("this scene repeats the previous one; move the story forward"),
	// not as a log line about the model.
	Reason string
	// Forbidden lists phrases or patterns the next attempt must avoid. Carried
	// on the retry annotation; ignored for other verdicts.
	Forbidden []string
}

// EvalGateConfig configures the post-hook returned by Engine.EvalGate.
//
// EXPERIMENTAL.
type EvalGateConfig struct {
	// Agents restricts the gate to these agent slugs. Empty means every agent,
	// which is rarely what you want: a gate that asks an author's quality
	// questions about a JSON extractor's output wastes a call and may fire
	// nonsense. Name the agents.
	Agents []string

	// Questions builds the question set for one step. Returning nil or an empty
	// map skips the evaluation entirely — no call is made and no cost is
	// incurred — which is how a gate cheaply opts out of the steps it does not
	// care about (a first turn with no previous scene to compare against, say).
	// Required.
	Questions func(req *StepRequest, res Result) map[string]EvalQuestion

	// State builds the value the questions are asked about. Nil uses the
	// result's text. Supply it when the questions need more than the output —
	// the action that prompted it, the previous turn, the session's facts — and
	// return a map so each question can refer to the part it is about by name.
	State func(req *StepRequest, res Result) any

	// Decide turns the answers into a verdict. Required.
	//
	// It receives miss-safe answers, so reading a question that was not
	// answered yields a zero value rather than a panic. Draw the thresholds
	// here and keep them in one place: this function is the whole policy, and
	// it is the thing you will tune.
	Decide func(EvalAnswers) EvalDecision

	// OnEvaluation observes every evaluation that ran, before the decision is
	// applied. Use it for cost accounting (Evaluation.Usage) and for logging
	// the numbers a threshold was drawn against. Optional; it must not block.
	OnEvaluation func(req *StepRequest, eval Evaluation)

	// FailClosed makes an evaluation ERROR fail the step. The default (false)
	// fails open: if the evaluator is unreachable or errors, the result passes
	// through as though the gate were not registered, so an outage in the
	// decision layer degrades quality rather than taking the product down.
	// Set it only for gates enforcing a rule you would rather serve nothing
	// than violate.
	FailClosed bool
}

// EvalGate returns a PostHook that evaluates a step's result and turns the
// answers into accept / retry / reject.
//
// With no Evaluator configured the hook is inert: it passes every result
// through untouched, so registering it unconditionally is safe and the feature
// can be switched on later by setting Config.Evaluator alone.
//
// A typical registration — one call, three independent questions, one policy:
//
//	e.Hooks().RegisterPost("quality", e.EvalGate(loom.EvalGateConfig{
//	    Agents: []string{"author"},
//	    State: func(req *loom.StepRequest, res loom.Result) any {
//	        return map[string]any{
//	            "draft":    loom.ResultText(res),
//	            "previous": req.Session.State.Vars["last_prose"],
//	        }
//	    },
//	    Questions: func(*loom.StepRequest, loom.Result) map[string]loom.EvalQuestion {
//	        return map[string]loom.EvalQuestion{
//	            "cliched": loom.EvalNoul("Does `draft` lean on stock clichés?"),
//	            "repeats": loom.EvalNoul("Does `draft` repeat the events of `previous` without advancing?"),
//	            "quality": loom.EvalScore("Rate the prose in `draft`",
//	                "generic filler", "competent but flat", "vivid and specific", "genuinely striking"),
//	        }
//	    },
//	    Decide: func(a loom.EvalAnswers) loom.EvalDecision {
//	        switch {
//	        case a.Yes("repeats", 0.7):
//	            return loom.EvalDecision{Verdict: loom.EvalRetry,
//	                Reason: "this scene repeats the previous one; advance the story"}
//	        case a.Yes("cliched", 0.7):
//	            return loom.EvalDecision{Verdict: loom.EvalRetry,
//	                Reason: "rewrite without stock clichés"}
//	        case a.Score("quality") < 1:
//	            return loom.EvalDecision{Verdict: loom.EvalRetry,
//	                Reason: "the prose is flat; be specific and concrete"}
//	        }
//	        return loom.EvalDecision{Verdict: loom.EvalAccept}
//	    },
//	}))
//
// EXPERIMENTAL.
func (e *Engine) EvalGate(cfg EvalGateConfig) PostHook {
	// A gate missing either required function would silently accept everything,
	// which is worse than an obvious failure: it looks registered and does
	// nothing. Fail the step instead, loudly, on the first call.
	if cfg.Questions == nil || cfg.Decide == nil {
		return func(context.Context, *StepRequest, Result) (Result, error) {
			return nil, fmt.Errorf("loom: EvalGate requires both Questions and Decide")
		}
	}

	var only map[string]struct{}
	if len(cfg.Agents) > 0 {
		only = make(map[string]struct{}, len(cfg.Agents))
		for _, s := range cfg.Agents {
			only[s] = struct{}{}
		}
	}

	return func(ctx context.Context, req *StepRequest, res Result) (Result, error) {
		if only != nil {
			if _, ok := only[req.AgentSlug]; !ok {
				return res, nil
			}
		}
		ev := e.Evaluator()
		if ev == nil {
			// Not opted in. The hook is a no-op rather than an error so it can
			// be registered unconditionally by code that does not know whether
			// the deployment has an evaluator.
			return res, nil
		}
		questions := cfg.Questions(req, res)
		if len(questions) == 0 {
			return res, nil
		}

		state := any(ResultText(res))
		if cfg.State != nil {
			state = cfg.State(req, res)
		}

		eval, err := ev.Evaluate(ctx, state, questions)
		if err != nil {
			if cfg.FailClosed {
				return nil, fmt.Errorf("loom: eval gate on %q: %w", req.AgentSlug, err)
			}
			// Fail open: the decision layer is down, so the result stands. Log
			// it — a gate that has silently stopped gating is exactly the kind
			// of failure that hides until someone reads the output.
			e.log.Error("eval gate failed open", "agent", req.AgentSlug, "err", err)
			return res, nil
		}
		if cfg.OnEvaluation != nil {
			cfg.OnEvaluation(req, eval)
		}

		switch d := cfg.Decide(eval.Answers); d.Verdict {
		case EvalRetry:
			return nil, ErrRetryWith(RetryAnnotation{Reason: d.Reason, Forbidden: d.Forbidden})
		case EvalReject:
			return nil, fmt.Errorf("loom: eval gate on %q rejected the result: %s", req.AgentSlug, d.Reason)
		default:
			return res, nil
		}
	}
}
