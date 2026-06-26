package loom

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/google/uuid"
)

// flow.go adds a multi-agent *turn* primitive on top of the single-agent step
// loop. A Flow declares a turn as one lead agent followed by any number of
// follower agents that run concurrently and consume the lead's output. This is
// the composition unit interactive-fiction style platforms need: a streaming
// "author" produces prose, then a "logician", "sensory director", "titler",
// etc. analyse that prose in parallel — all as one logical turn.
//
// Every agent in a flow is an ordinary versioned loom Agent, so the only thing
// that changes between products (or between text, image, video, spatial, and AR
// modalities) is the set of agents/prompts referenced by the Flow plus the
// hooks registered on the engine. The engine machinery — sessions, retries,
// cost, branching, persistence — is unchanged.

// FlowAgent is one agent participating in a turn.
type FlowAgent struct {
	// AgentSlug + AgentVersion address a registered agent (version 0 = latest).
	AgentSlug    string
	AgentVersion int
	// Stream, on the lead agent only, routes streaming chunks to TurnRequest.OnChunk.
	Stream bool
	// OutputKey is the key under which this agent's text output is exposed to
	// later agents in the same turn via {{.Inputs.<OutputKey>}}. For the lead it
	// defaults to "Lead"; for followers it is optional.
	OutputKey string
	// Overrides is opaque per-call data forwarded to the generator (e.g. a
	// per-user api key / model). Optional.
	Overrides map[string]any
	// GeneratorOverride routes this agent's call to a different registered
	// generator for this turn only. Optional.
	GeneratorOverride string
	// Params are this agent's tuning knobs (model + domain), merged over the
	// turn's Params. See StepRequest.Params.
	Params map[string]any
}

// Flow declares a turn as a lead agent plus parallel followers.
type Flow struct {
	Slug      string
	Lead      FlowAgent
	Followers []FlowAgent
}

// TurnRequest is the input to RunTurn.
type TurnRequest struct {
	Flow Flow
	// Action is the player/user input that drives the turn (nil for an opening).
	Action *Action
	// OnChunk receives streaming fragments from the lead agent (if Lead.Stream).
	OnChunk func(Chunk)
	// Inputs seeds values available to every agent in the turn via
	// {{.Inputs.<key>}}. The lead's output is added under its OutputKey before
	// followers run.
	Inputs map[string]any
	// Overrides is opaque per-turn data forwarded to every agent's generator
	// unless an individual FlowAgent specifies its own Overrides.
	Overrides map[string]any
	// Params are turn-wide tuning knobs (model + domain, e.g. temperature,
	// tension) merged under each FlowAgent's own Params.
	Params map[string]any
}

// Turn is the result of RunTurn: the lead step plus follower steps, all sharing
// a single turn ID (recorded on each Step's Diagnostics as "turn_id").
type Turn struct {
	ID        uuid.UUID
	Lead      *Step
	Followers map[string]*Step // keyed by follower AgentSlug
	Steps     []*Step          // lead first, then followers (completion order)
	Messages  []Message        // everything published on the turn's Bus
}

// RunTurn executes a Flow against a session: it runs the lead agent (optionally
// streaming), injects the lead's text output into the shared Inputs, then runs
// all follower agents concurrently. Every resulting step is persisted and tagged
// with the same turn ID. A follower failure does not abort the turn — its error
// is recorded on the Turn but siblings still complete.
func (e *Engine) RunTurn(ctx context.Context, session *Session, req TurnRequest) (*Turn, error) {
	turnID := uuid.New()

	inputs := map[string]any{}
	maps.Copy(inputs, req.Inputs)
	// Expose the turn's action text to every agent (lead and followers) under
	// "action" unless the caller already supplied it.
	if _, ok := inputs["action"]; !ok {
		if t := actionText(req.Action); t != "" {
			inputs["action"] = t
		}
	}

	// Per-turn cross-agent channel fabric, available to every agent's generator.
	bus := newBus()
	defer bus.close()

	// ---- Lead ----
	leadKey := req.Flow.Lead.OutputKey
	if leadKey == "" {
		leadKey = "Lead"
	}
	leadSlug := req.Flow.Lead.AgentSlug
	// The lead's chunks are published on the "lead" topic as they stream, so a
	// follower can consume the lead's output live (and, via replay, even though
	// followers start after the lead).
	var onChunk func(Chunk)
	if req.Flow.Lead.Stream {
		onChunk = func(c Chunk) {
			bus.Publish(leadSlug, "lead", c.Content)
			if req.OnChunk != nil {
				req.OnChunk(c)
			}
		}
	}
	leadStep, err := e.steps.run(ctx, session, StepRequest{
		AgentSlug:         req.Flow.Lead.AgentSlug,
		AgentVersion:      req.Flow.Lead.AgentVersion,
		Action:            req.Action,
		OnChunk:           onChunk,
		Inputs:            cloneInputs(inputs),
		Params:            mergeParams(req.Params, req.Flow.Lead.Params),
		Overrides:         pickOverrides(req.Flow.Lead.Overrides, req.Overrides),
		GeneratorOverride: req.Flow.Lead.GeneratorOverride,
		turnID:            turnID,
		turnRole:          "lead",
		bus:               bus,
	})
	if err != nil {
		return nil, fmt.Errorf("loom: turn %q lead %q: %w", req.Flow.Slug, req.Flow.Lead.AgentSlug, err)
	}
	inputs[leadKey] = ResultText(leadStep.Result)
	bus.Publish(leadSlug, "lead.done", ResultText(leadStep.Result))

	turn := &Turn{
		ID:        turnID,
		Lead:      leadStep,
		Followers: map[string]*Step{},
		Steps:     []*Step{leadStep},
	}

	// ---- Followers (concurrent) ----
	// The shared inputs map is only READ from here on: the lead's output was
	// injected above, before any goroutine starts, and nothing writes back into
	// it during the fan-out. So each follower's cloneInputs is its own private
	// copy with no write to race against.
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, f := range req.Flow.Followers {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			step, ferr := e.steps.run(ctx, session, StepRequest{
				AgentSlug:    f.AgentSlug,
				AgentVersion: f.AgentVersion,
				// The turn's Action is recorded once, on the lead step. Followers
				// analyse the lead's output and read the action via Inputs, so they
				// carry no Action (sharing the lead's would duplicate its ID).
				Action:            nil,
				Inputs:            cloneInputs(inputs),
				Params:            mergeParams(req.Params, f.Params),
				Overrides:         pickOverrides(f.Overrides, req.Overrides),
				GeneratorOverride: f.GeneratorOverride,
				turnID:            turnID,
				turnRole:          "follower:" + f.AgentSlug,
				bus:               bus,
			})
			if ferr == nil {
				// Publish the follower's output so concurrent siblings can react.
				bus.Publish(f.AgentSlug, f.AgentSlug, ResultText(step.Result))
			}
			mu.Lock()
			defer mu.Unlock()
			if ferr != nil {
				e.log.Error("turn follower failed", "turn_id", turnID, "agent", f.AgentSlug, "err", ferr)
				return
			}
			turn.Followers[f.AgentSlug] = step
			turn.Steps = append(turn.Steps, step)
		}()
	}
	wg.Wait()

	// The turn's dominant modality is the lead's output.
	session.State.Modality = leadStep.Result.Modality()

	turn.Messages = bus.Messages()
	return turn, nil
}

// mergeParams overlays per-agent params on turn-wide defaults (agent wins).
func mergeParams(turnParams, agentParams map[string]any) map[string]any {
	if len(turnParams) == 0 && len(agentParams) == 0 {
		return nil
	}
	out := make(map[string]any, len(turnParams)+len(agentParams))
	maps.Copy(out, turnParams)
	maps.Copy(out, agentParams)
	return out
}

// ResultText extracts the primary text payload from a Result for prompt
// injection, returning "" for modalities without a natural text form.
func ResultText(r Result) string {
	switch v := r.(type) {
	case *TextResult:
		return v.Content
	case *StructuredResult:
		if s, ok := v.Data["text"].(string); ok {
			return s
		}
	case *ImageResult:
		return v.Prompt
	case *VideoResult:
		return v.PreviewImage
	}
	return ""
}

// actionText best-effort extracts a human-readable string from an Action for
// prompt/input injection. It understands the common free-text payload shape.
func actionText(a *Action) string {
	if a == nil {
		return ""
	}
	switch p := a.Payload.(type) {
	case map[string]any:
		if s, ok := p["text"].(string); ok {
			return s
		}
	case FreeText:
		return p.Body
	case string:
		return p
	}
	return ""
}

func cloneInputs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

func pickOverrides(specific, fallback map[string]any) map[string]any {
	if specific != nil {
		return specific
	}
	return fallback
}
