package narrative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rhaqim/conexus-refactor/domain"
	loom "github.com/rhaqim/loom"
)

// Flow builds the turn definition at a target prompt version: the Author streams
// prose (lead), then the Logician and Sensory directors analyse it in parallel
// (followers). Each agent's version is resolved independently to the highest
// available version <= target, so you can iterate ONE prompt (e.g. drop
// author-*.v2.txt) and run -v 2 while the others stay at v1.
func Flow(versions map[string][]int, target int) loom.Flow {
	return loom.Flow{
		Slug: fmt.Sprintf("story-turn-v%d", target),
		// Per-agent params: the Author runs hot for creative prose; the JSON
		// agents run cold for reliable structured output. Turn-wide params (e.g.
		// tension) merge under these — see engine.PlayTurn.
		Lead: loom.FlowAgent{AgentSlug: AgentAuthor, AgentVersion: ResolveVersion(versions, AgentAuthor, target), Stream: true, OutputKey: "Prose",
			Params: map[string]any{"temperature": 0.85}},
		Followers: []loom.FlowAgent{
			{AgentSlug: AgentLogician, AgentVersion: ResolveVersion(versions, AgentLogician, target),
				Params: map[string]any{"temperature": 0.2}},
			{AgentSlug: AgentSensory, AgentVersion: ResolveVersion(versions, AgentSensory, target),
				Params: map[string]any{"temperature": 0.5}},
		},
	}
}

// ResolveVersion returns the highest available version of agent that is <= target;
// if none qualifies it returns the lowest available, or the target as a last resort.
func ResolveVersion(versions map[string][]int, agent string, target int) int {
	vs := versions[agent]
	if len(vs) == 0 {
		return target
	}
	best := 0
	for _, v := range vs {
		if v <= target && v > best {
			best = v
		}
	}
	if best == 0 {
		return vs[0]
	}
	return best
}

// ParseLogician extracts a LogicianOutput from a model's (possibly fenced) JSON.
func ParseLogician(text string) (domain.LogicianOutput, error) {
	var out domain.LogicianOutput
	err := json.Unmarshal([]byte(extractJSON(text)), &out)
	return out, err
}

// ParseSensory extracts a SensoryOutput from a model's JSON, accepting both the
// flat v1 shape ({image_prompt, style_lock, music_mood, intensity}) and the real
// conexus v2 schema ({image:{prompt,...}, music:{mood,intensity,...}}).
func ParseSensory(text string) (domain.SensoryOutput, error) {
	raw := []byte(extractJSON(text))
	var flat domain.SensoryOutput
	if err := json.Unmarshal(raw, &flat); err == nil && flat.ImagePrompt != "" {
		return flat, nil
	}
	var nested struct {
		Image struct {
			Prompt    string `json:"prompt"`
			StyleLock string `json:"style_lock"`
		} `json:"image"`
		Music struct {
			Mood      string  `json:"mood"`
			Intensity float64 `json:"intensity"`
		} `json:"music"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		return flat, err
	}
	return domain.SensoryOutput{
		ImagePrompt: nested.Image.Prompt,
		StyleLock:   nested.Image.StyleLock,
		MusicMood:   nested.Music.Mood,
		Intensity:   nested.Music.Intensity,
	}, nil
}

// MigrationResult is the parsed output of the migration agent.
type MigrationResult struct {
	World        domain.World        `json:"world"`
	Constitution domain.Constitution `json:"constitution"`
}

// ParseMigration extracts a World + Constitution from the migrate agent's JSON.
func ParseMigration(text string) (MigrationResult, error) {
	var out MigrationResult
	err := json.Unmarshal([]byte(extractJSON(text)), &out)
	return out, err
}

// JSONOf returns the JSON text of a result, whether the provider honored
// response_format (a StructuredResult carrying parsed Data) or returned plain
// text (a TextResult). This keeps parsing uniform across OpenAI-compatible
// providers regardless of response_format support.
func JSONOf(res loom.Result) string {
	switch r := res.(type) {
	case *loom.StructuredResult:
		b, _ := json.Marshal(r.Data)
		return string(b)
	case *loom.TextResult:
		return r.Content
	default:
		return loom.ResultText(res)
	}
}

// extractJSON strips ```json fences and leading/trailing prose, returning the
// outermost {...} block.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			return s[i : j+1]
		}
	}
	return s
}

// QAHook validates the Logician's output and asks loom to retry the agent when it
// is malformed or low-quality. This is the deterministic-QA-as-a-hook pattern:
// invalid JSON, wrong option count, or duplicate verb buckets all trigger a
// retry, and the next attempt sees the reason on req.Annotations. Non-logician
// agents pass through untouched.
func QAHook() loom.PostHook {
	return func(_ context.Context, req *loom.StepRequest, res loom.Result) (loom.Result, error) {
		if req.AgentSlug != AgentLogician {
			return res, nil
		}
		out, err := ParseLogician(JSONOf(res))
		if err != nil {
			return nil, loom.ErrRetryWith(loom.RetryAnnotation{
				Reason: "your previous output was not valid JSON; return ONLY the JSON object",
			})
		}
		if len(out.Options) != 3 {
			return nil, loom.ErrRetryWith(loom.RetryAnnotation{
				Reason: fmt.Sprintf("return EXACTLY 3 options (you returned %d)", len(out.Options)),
			})
		}
		seen := map[string]int{}
		for _, o := range out.Options {
			seen[firstWord(o.Label)]++
		}
		var dup []string
		for verb, n := range seen {
			if verb != "" && n > 1 {
				dup = append(dup, verb)
			}
		}
		if len(dup) > 0 {
			return nil, loom.ErrRetryWith(loom.RetryAnnotation{
				Reason:    fmt.Sprintf("two options share a verb bucket (%v); make each option start with a different strong verb", dup),
				Forbidden: dup,
			})
		}
		return res, nil
	}
}

func firstWord(s string) string {
	f := strings.Fields(strings.ToLower(s))
	if len(f) == 0 {
		return ""
	}
	return strings.Trim(f[0], ".,!?;:'\"")
}

// FrameAction turns a chosen option or free text into a story beat for the Author.
func FrameAction(action, protagonist string) string {
	action = strings.TrimSpace(action)
	if action == "" {
		return ""
	}
	return fmt.Sprintf("%s decides to: %s", protagonist, action)
}
