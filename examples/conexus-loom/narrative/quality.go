package narrative

import (
	"context"
	"fmt"
	"strings"

	loom "github.com/rhaqim/loom"
)

// quality.go is the pre-process Quality Engine: deterministic post-hooks that
// reject low-quality Author prose and ask loom to retry, mirroring the proven
// Logician QAHook pattern. The Choice Directive policy is already the registered
// QAHook (exactly-N options + verb-bucket dedupe); the hooks here add Slop Bans
// and a Staleness Detector. They run only on the Author and pass everything else
// through untouched.

// defaultSlopPhrases is a conservative set of overused narration clichés. Keep it
// specific to avoid false-positive retries.
var defaultSlopPhrases = []string{
	"a chill ran down", "sent shivers down", "shivers down her spine", "shivers down his spine",
	"little did", "in that moment", "time seemed to slow", "time stood still",
	"barely above a whisper", "couldn't help but", "the air was thick with",
	"a single tear", "breath caught", "let out a breath she didn't know",
	"unbeknownst to", "dark and stormy night", "calm before the storm",
	"heart pounded in", "heart raced", "a mixture of fear and",
}

// SlopBanHook rejects Author prose that contains any banned cliché phrase and
// asks loom to retry, passing the offending phrases as Forbidden so the next
// attempt avoids them. With no phrases supplied it uses a built-in list.
func SlopBanHook(banned ...string) loom.PostHook {
	if len(banned) == 0 {
		banned = defaultSlopPhrases
	}
	lowered := make([]string, len(banned))
	for i, p := range banned {
		lowered[i] = strings.ToLower(p)
	}
	return func(_ context.Context, req *loom.StepRequest, res loom.Result) (loom.Result, error) {
		if req.AgentSlug != AgentAuthor {
			return res, nil
		}
		text := strings.ToLower(loom.ResultText(res))
		var hits []string
		for i, p := range lowered {
			if strings.Contains(text, p) {
				hits = append(hits, banned[i])
			}
		}
		if len(hits) > 0 {
			return nil, loom.ErrRetryWith(loom.RetryAnnotation{
				Reason:    "your prose used banned cliché phrases; rewrite the scene without them",
				Forbidden: hits,
			})
		}
		return res, nil
	}
}

// StalenessHook rejects Author prose that overlaps the previous turn's prose by
// at least threshold (word-trigram Jaccard), asking loom to retry with guidance
// to advance the story. threshold <= 0 defaults to 0.7.
func StalenessHook(threshold float64) loom.PostHook {
	if threshold <= 0 {
		threshold = 0.7
	}
	return func(_ context.Context, req *loom.StepRequest, res loom.Result) (loom.Result, error) {
		if req.AgentSlug != AgentAuthor || req.Session == nil {
			return res, nil
		}
		prev, _ := req.Session.State.Vars["last_prose"].(string)
		if prev == "" {
			return res, nil
		}
		if overlap := trigramOverlap(prev, loom.ResultText(res)); overlap >= threshold {
			return nil, loom.ErrRetryWith(loom.RetryAnnotation{
				Reason: fmt.Sprintf("this scene repeats the previous one (%.0f%% overlap); advance with a new location, fact, character, or development", overlap*100),
			})
		}
		return res, nil
	}
}

// trigramOverlap is the Jaccard similarity of the two texts' word trigrams (0..1).
func trigramOverlap(a, b string) float64 {
	ta, tb := wordTrigrams(a), wordTrigrams(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func wordTrigrams(s string) map[string]bool {
	words := strings.Fields(strings.ToLower(s))
	out := make(map[string]bool)
	for i := 0; i+2 < len(words); i++ {
		out[words[i]+" "+words[i+1]+" "+words[i+2]] = true
	}
	return out
}
