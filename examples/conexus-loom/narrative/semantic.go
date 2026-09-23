package narrative

import (
	loom "github.com/rhaqim/loom"
)

// semantic.go is the Quality Engine's semantic tier, built on loom's
// EXPERIMENTAL evaluation subsystem. It is the same job as quality.go —
// reject weak Author prose and ask loom to retry — but asked as questions
// instead of matched as strings.
//
// The split is deliberate, and it is the whole point of the layering: loom owns
// the MECHANISM (an Evaluator interface, a batch call, EvalGate turning answers
// into retries) and knows nothing about fiction. This file owns the QUESTIONS —
// what "slop", "stale" and "good prose" mean for this product. Questions are
// prompts, and prompts have always lived here.
//
// What it buys over quality.go:
//
//   - SlopBanHook matches 21 hard-coded phrases, so it catches exactly the
//     clichés someone remembered to type. A question catches the ones they did
//     not.
//   - StalenessHook is word-trigram Jaccard, so it catches copy-paste but not
//     the same scene rewritten in different words — which is the repetition
//     that actually happens.
//   - The verdict stops being binary. A weighted score lets a merely flat draft
//     ship (and be logged) while a genuinely bad one is sent back.
//
// All three questions ride in ONE request and are evaluated independently, so
// this costs about as much as asking any one of them.

// Question ids, shared between the question set and the policy so a typo cannot
// silently turn a gate off — a missing answer reads as a zero value.
const (
	qCliched = "cliched"
	qRepeats = "repeats"
	qQuality = "quality"
)

// proseLevels is the Author's rubric, lowest first. The levels describe what a
// reader would actually notice; "moderately good" would not separate anything.
var proseLevels = []string{
	"Generic filler: could open any story, nothing specific to this world or character.",
	"Competent but flat: correct and readable, with no image or detail worth remembering.",
	"Vivid and specific: concrete sensory detail rooted in this scene.",
	"Genuinely striking: a line or image a reader would quote back.",
}

// SemanticQualityGate returns the Author-scoped post-hook. It asks three
// questions about the draft and turns the answers into accept / retry.
//
// minQuality is the weighted score below which a draft is sent back, on the
// 0..3 proseLevels scale. Zero disables the quality tier and leaves the two
// yes/no gates in force; 1.0 is a reasonable starting point (reject anything
// below "competent but flat").
//
// The hook is inert on an engine with no Evaluator configured, so registering it
// unconditionally is safe — see loom's EvalGate.
func SemanticQualityGate(e *loom.Engine, minQuality float64) loom.PostHook {
	return e.EvalGate(loom.EvalGateConfig{
		Agents: []string{AgentAuthor},

		// The questions are asked about the draft AND the previous scene, as
		// named fields, so the repetition question can point at exactly the
		// thing it compares against.
		State: func(req *loom.StepRequest, res loom.Result) any {
			state := map[string]any{"draft": loom.ResultText(res)}
			if req.Session != nil {
				if prev, _ := req.Session.State.Vars["last_prose"].(string); prev != "" {
					state["previous_scene"] = prev
				}
			}
			return state
		},

		Questions: func(req *loom.StepRequest, _ loom.Result) map[string]loom.EvalQuestion {
			qs := map[string]loom.EvalQuestion{
				qCliched: loom.EvalNoulWith(
					"Does `draft` lean on stock clichés, stale imagery, or stock emotional beats?",
					"The prose reaches for familiar, overused phrasing.",
					"The prose finds its own images and phrasing.",
				),
				qQuality: loom.EvalScore("Rate the prose in `draft`", proseLevels...),
			}
			// The first scene has nothing to repeat. Leaving the question out
			// rather than asking it against an empty string keeps the answer
			// honest and costs nothing.
			if req.Session != nil {
				if prev, _ := req.Session.State.Vars["last_prose"].(string); prev != "" {
					qs[qRepeats] = loom.EvalNoulWith(
						"Does `draft` retell the events of `previous_scene` without advancing the story?",
						"The scene covers the same ground: no new location, fact, character, or development.",
						"The scene moves the story somewhere the previous one did not.",
					)
				}
			}
			return qs
		},

		Decide: func(a loom.EvalAnswers) loom.EvalDecision {
			// Ordered by how specific the guidance can be: a precise instruction
			// gives the next attempt more to work with than "try harder".
			switch {
			case a.Yes(qRepeats, 0.7):
				return loom.EvalDecision{
					Verdict: loom.EvalRetry,
					Reason:  "this scene repeats the previous one; advance with a new location, fact, character, or development",
				}
			case a.Yes(qCliched, 0.7):
				return loom.EvalDecision{
					Verdict: loom.EvalRetry,
					Reason:  "your prose leans on stock clichés and stale imagery; rewrite the scene with your own images",
				}
			case minQuality > 0 && a.Has(qQuality) && a.Score(qQuality) < minQuality:
				return loom.EvalDecision{
					Verdict: loom.EvalRetry,
					Reason:  "the prose is generic; ground the scene in concrete, specific sensory detail",
				}
			}
			return loom.EvalDecision{Verdict: loom.EvalAccept}
		},
	})
}
