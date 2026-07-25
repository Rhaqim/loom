package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// flow.go adds a multi-agent *turn* primitive on top of the single-agent step
// loop. A Flow declares a turn as one lead agent followed by any number of
// follower agents. Followers run in dependency layers — concurrently within a
// layer, and after any sibling whose output they consume — so the default of
// independent followers is a single concurrent fan-out. This is the composition
// unit interactive-fiction style platforms need: a streaming "author" produces
// prose, then a "logician", "sensory director", "titler", etc. analyse that
// prose in parallel (and, where wired, feed each other) — all as one turn.
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
	// RetryMode and MaxRetries override the engine retry defaults for this
	// agent's step (e.g. a keep-best lead, a discard follower). Zero values
	// inherit the engine default.
	RetryMode  RetryMode
	MaxRetries int
	// SystemPrompt, when non-zero, is this agent's per-turn system prompt
	// override (typically PromptRef{Literal: ...} assembled by the embedder).
	// The zero value leaves the agent's registered system prompt in place.
	SystemPrompt PromptRef
	// Inputs are merged over the turn's shared Inputs for this agent only, so
	// each agent can receive its own user_prompt / values while still seeing the
	// lead's output. Nil leaves the shared Inputs untouched.
	Inputs map[string]any
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
	// Owner scopes registry resolution for every agent in the turn — the lead
	// and all followers — exactly as StepRequest.Owner does for a single step.
	// "" is the global scope. RunTurnBySlug sets this from the flow's own owner
	// so a persisted flow always resolves its agents within its own tenant.
	Owner string
	// Action is the user input that drives the turn (nil for an opening).
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
	// OnStreamEnd fires when the lead's streamed draft is complete, before the
	// lead's post-hooks run, carrying the attempt index. It lets the caller close
	// its chunk sink the moment attempt 0 finishes even if a silent keep-best
	// retry follows. See StepRequest.OnStreamEnd.
	OnStreamEnd func(attempt int)
	// OnStep fires as each agent's step settles: once for the lead (role "lead",
	// err nil) guaranteed before any follower goroutine starts, then once per
	// follower (role "follower:<slug>") on success (step set, err nil) or failure
	// (step nil, err set), from the follower's own goroutine and outside the
	// turn's mutex. The callback MUST NOT block — a slow OnStep would stall the
	// turn. It lets an embedder build follower inputs from the lead and react to
	// each follower in completion order.
	OnStep func(role string, step *Step, err error)
}

// Turn is the result of RunTurn: the lead step plus follower steps, all sharing
// a single turn ID (recorded on each Step's Diagnostics as "turn_id").
type Turn struct {
	ID        uuid.UUID
	Lead      *Step
	Followers map[string]*Step // keyed by follower AgentSlug (successes only)
	Errors    map[string]error // keyed by follower AgentSlug (failures only)
	Steps     []*Step          // lead first, then followers (completion order)
	Messages  []Message        // everything published on the turn's Bus
}

// RunTurn executes a Flow against a session: it runs the lead agent (optionally
// streaming), injects the lead's text output into the shared Inputs, then runs
// the follower agents. Every resulting step is persisted and tagged with the
// same turn ID. A follower failure does not abort the turn — its error is
// recorded on the Turn but siblings still complete.
//
// Followers run in dependency LAYERS: a follower that consumes another
// follower's output (its user template declares the sibling's OutputKey in
// Prompt.Variables) runs in a later layer, after that producer, and receives
// its output under the producer's OutputKey. Followers within a layer run
// concurrently, and a flow with no cross-follower dependencies is a single
// layer — identical to the historical all-concurrent fan-out. Validate the
// wiring with Flows().Validate; a dependency cycle is rejected there, and if an
// in-code flow still contains one, the cyclic followers run last and fail on
// their missing input rather than deadlocking.
//
// A non-nil error means the lead step failed; it is the lead's RunStep error
// (see RunStep for the error contract) wrapped with the turn and agent slug.
// Follower failures are never returned here — inspect Turn.Errors (keyed by
// agent slug) for those.
//
// ErrSessionNotPersisted is the exception: it means the step itself ran and
// committed and only the session row is stale, so the turn is NOT aborted. The
// step appears in Turn.Steps (and Turn.Followers) as normal and the error is
// recorded in Turn.Errors under "lead" or the follower's agent slug. A turn can
// therefore return a nil error while carrying entries in Turn.Errors — check it
// before treating Session.State as authoritative.
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
		AgentSlug:            req.Flow.Lead.AgentSlug,
		AgentVersion:         req.Flow.Lead.AgentVersion,
		Owner:                req.Owner,
		Action:               req.Action,
		OnChunk:              onChunk,
		OnStreamEnd:          req.OnStreamEnd,
		RetryMode:            req.Flow.Lead.RetryMode,
		MaxRetries:           req.Flow.Lead.MaxRetries,
		SystemPromptOverride: promptRefOf(req.Flow.Lead.SystemPrompt),
		Inputs:               agentInputs(inputs, req.Flow.Lead.Inputs),
		Params:               mergeParams(req.Params, req.Flow.Lead.Params),
		Overrides:            pickOverrides(req.Flow.Lead.Overrides, req.Overrides),
		GeneratorOverride:    req.Flow.Lead.GeneratorOverride,
		turnID:               turnID,
		turnRole:             "lead",
		bus:                  bus,
	})
	// ErrSessionNotPersisted means the lead step itself ran and committed — only
	// the session row is stale. The step is valid, so the turn proceeds and the
	// error is surfaced on Turn.Errors rather than aborting siblings that would
	// otherwise have succeeded.
	var leadNotPersisted error
	if errors.Is(err, ErrSessionNotPersisted) {
		leadNotPersisted, err = err, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loom: turn %q lead %q: %w", req.Flow.Slug, req.Flow.Lead.AgentSlug, err)
	}
	inputs[leadKey] = ResultText(leadStep.Result)
	bus.Publish(leadSlug, "lead.done", ResultText(leadStep.Result))

	turn := &Turn{
		ID:        turnID,
		Lead:      leadStep,
		Followers: map[string]*Step{},
		Errors:    map[string]error{},
		Steps:     []*Step{leadStep},
	}
	// The lead ran, but its session row write did not land. The step stands; the
	// caller is told under the "lead" key so it can reconcile before trusting
	// Session.State.
	if leadNotPersisted != nil {
		turn.Errors["lead"] = leadNotPersisted
		e.log.Error("turn lead session row not persisted", "turn_id", turnID, "agent", leadSlug, "err", leadNotPersisted)
	}
	// Fire the lead callback synchronously, before any follower goroutine starts,
	// so an embedder can build the follower inputs from the lead's output without
	// racing the fan-out.
	if req.OnStep != nil {
		req.OnStep("lead", leadStep, nil)
	}

	// ---- Followers (dependency-layered) ----
	// Followers run concurrently within a layer; a follower that consumes another
	// follower's output runs in a later layer so its producer has finished. A
	// flow with no cross-follower dependencies is a single layer — identical to
	// the historical all-concurrent fan-out.
	//
	// Within a layer the shared inputs map is only READ (each follower clones it
	// via agentInputs), and it is written ONLY at a layer barrier below, when no
	// follower goroutine is running — so the map write never races a read.
	followers := req.Flow.Followers
	outputKeys := make([]string, len(followers))
	consumed := make([][]string, len(followers))
	for i, f := range followers {
		outputKeys[i] = f.OutputKey
		consumed[i] = e.userTemplateVars(ctx, req.Owner, f.AgentSlug, f.AgentVersion)
	}
	layers, cyclic := topoLayers(outputKeys, consumed)
	if len(cyclic) > 0 {
		// Validate rejects a cycle at publish time; an in-code flow can still
		// reach here. Run the cyclic followers last so each fails on its missing
		// sibling input (ErrMissingVariables) rather than deadlocking the turn.
		e.log.Error("turn: follower dependency cycle; running cyclic followers last",
			"turn_id", turnID, "count", len(cyclic))
		layers = append(layers, cyclic)
	}

	var mu sync.Mutex
	runFollower := func(f FlowAgent) {
		role := "follower:" + f.AgentSlug
		fired := false
		// A follower panic (in the generator, a hook, or an OnStep callback)
		// must degrade only this follower, not crash the whole process. Record
		// it as a follower error and, if the normal path had not already fired
		// OnStep, surface it there too.
		defer func() {
			if r := recover(); r != nil {
				perr := fmt.Errorf("loom: follower %q panicked: %v", f.AgentSlug, r)
				mu.Lock()
				if _, ok := turn.Followers[f.AgentSlug]; !ok {
					turn.Errors[f.AgentSlug] = perr
				}
				mu.Unlock()
				e.log.Error("turn follower panicked", "turn_id", turnID, "agent", f.AgentSlug, "err", perr)
				if req.OnStep != nil && !fired {
					req.OnStep(role, nil, perr)
				}
			}
		}()
		step, ferr := e.steps.run(ctx, session, StepRequest{
			AgentSlug:    f.AgentSlug,
			AgentVersion: f.AgentVersion,
			Owner:        req.Owner,
			// The turn's Action is recorded once, on the lead step. Followers
			// analyse the lead's output and read the action via Inputs, so they
			// carry no Action (sharing the lead's would duplicate its ID).
			Action:               nil,
			RetryMode:            f.RetryMode,
			MaxRetries:           f.MaxRetries,
			SystemPromptOverride: promptRefOf(f.SystemPrompt),
			Inputs:               agentInputs(inputs, f.Inputs),
			Params:               mergeParams(req.Params, f.Params),
			Overrides:            pickOverrides(f.Overrides, req.Overrides),
			GeneratorOverride:    f.GeneratorOverride,
			turnID:               turnID,
			turnRole:             role,
			bus:                  bus,
		})
		// ErrSessionNotPersisted leaves a valid, committed step behind — only
		// the session row is stale. Treat the step as a success (publish it,
		// record it) while still reporting the error, so a stale row does not
		// silently drop a follower's output from the turn.
		notPersisted := errors.Is(ferr, ErrSessionNotPersisted)
		if ferr == nil || notPersisted {
			// Publish the follower's output so concurrent siblings can react.
			bus.Publish(f.AgentSlug, f.AgentSlug, ResultText(step.Result))
		}
		// Record the outcome under the mutex (map/slice writes only)...
		mu.Lock()
		if ferr != nil {
			turn.Errors[f.AgentSlug] = ferr
			e.log.Error("turn follower failed", "turn_id", turnID, "agent", f.AgentSlug, "err", ferr)
		}
		if ferr == nil || notPersisted {
			turn.Followers[f.AgentSlug] = step
			turn.Steps = append(turn.Steps, step)
		}
		mu.Unlock()
		// ...then fire OnStep OUTSIDE the mutex so a slow callback on one
		// follower cannot serialize the others.
		if req.OnStep != nil {
			fired = true
			switch {
			case notPersisted:
				// Valid step, stale session row — hand back both.
				req.OnStep(role, step, ferr)
			case ferr != nil:
				req.OnStep(role, nil, ferr)
			default:
				req.OnStep(role, step, nil)
			}
		}
	}

	for _, layer := range layers {
		var wg sync.WaitGroup
		for _, idx := range layer {
			f := followers[idx]
			wg.Add(1)
			go func() {
				defer wg.Done()
				runFollower(f)
			}()
		}
		wg.Wait()
		// Barrier: publish this layer's outputs into the shared inputs so the
		// next layer's templates can read them. Single-threaded here (all layer
		// goroutines have returned), so the map write cannot race a follower.
		// A follower that failed is absent from turn.Followers, so its output is
		// simply not injected — a consumer that declared it then fails at render
		// with ErrMissingVariables, attributing the missing dependency.
		for _, idx := range layer {
			f := followers[idx]
			if f.OutputKey == "" {
				continue
			}
			if step, ok := turn.Followers[f.AgentSlug]; ok {
				inputs[f.OutputKey] = ResultText(step.Result)
			}
		}
	}

	// The turn's dominant modality is the lead's output.
	session.State.Modality = leadStep.Result.Modality()

	turn.Messages = bus.Messages()
	return turn, nil
}

// topoLayers partitions N followers into dependency layers by Kahn's algorithm.
// outputKeys[i] is follower i's output name (empty = it produces nothing);
// consumed[i] is the input names follower i consumes. An edge j→i exists when
// follower i consumes follower j's output key, meaning i must run after j.
//
// Layer 0 holds followers that depend on no sibling; each later layer holds
// followers whose sibling producers all appear in an earlier layer. Consumed
// names that match no follower output key (external inputs, the lead's output)
// create no edge — the lead has already run and externals are present from the
// start, so they never constrain follower order.
//
// Followers caught in a dependency cycle cannot be ordered and are returned in
// `cyclic`; the caller decides what to do with them (Validate rejects the flow,
// RunTurn runs them in a final layer so they fail on their missing input rather
// than deadlock). Layer contents are sorted by index for deterministic order.
func topoLayers(outputKeys []string, consumed [][]string) (layers [][]int, cyclic []int) {
	n := len(outputKeys)
	producer := map[string]int{} // output name -> producing follower index
	for i, k := range outputKeys {
		if k != "" {
			producer[k] = i
		}
	}
	dependents := make([][]int, n) // j -> followers that depend on j
	indeg := make([]int, n)
	for i := range consumed {
		seen := map[int]bool{}
		for _, name := range consumed[i] {
			j, ok := producer[name]
			if !ok || j == i || seen[j] {
				continue
			}
			seen[j] = true
			dependents[j] = append(dependents[j], i)
			indeg[i]++
		}
	}

	var cur []int
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			cur = append(cur, i)
		}
	}
	placed := 0
	for len(cur) > 0 {
		sort.Ints(cur)
		layers = append(layers, cur)
		var next []int
		for _, j := range cur {
			placed++
			for _, i := range dependents[j] {
				indeg[i]--
				if indeg[i] == 0 {
					next = append(next, i)
				}
			}
		}
		cur = next
	}
	if placed < n {
		for i := 0; i < n; i++ {
			if indeg[i] > 0 {
				cyclic = append(cyclic, i)
			}
		}
	}
	return layers, cyclic
}

// userTemplateVars best-effort resolves the input-variable names an agent's user
// template declares (its Prompt.Variables), used to build the follower
// dependency graph. It returns nil on any resolution failure — a missing agent
// or prompt means "unknown dependencies", so the follower lands in the first
// layer and surfaces its own error when it actually runs, preserving RunTurn's
// per-follower failure isolation.
func (e *Engine) userTemplateVars(ctx context.Context, owner, slug string, version int) []string {
	agent, err := e.agents.Get(ctx, owner, slug, version)
	if err != nil || agent.UserTemplateID == uuid.Nil {
		return nil
	}
	ut, err := e.prompts.GetByID(ctx, agent.UserTemplateID)
	if err != nil {
		return nil
	}
	return ut.Variables
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

// agentInputs clones the shared turn inputs and merges the agent's own Inputs on
// top, so each agent can carry its own values (e.g. a distinct user_prompt)
// without disturbing siblings. cloneInputs always returns a non-nil map.
func agentInputs(shared, own map[string]any) map[string]any {
	m := cloneInputs(shared)
	maps.Copy(m, own)
	return m
}

// promptRefOf returns a pointer to ref when it carries an override, or nil when
// it is the zero value (so the agent's registered system prompt is used).
func promptRefOf(ref PromptRef) *PromptRef {
	if ref == (PromptRef{}) {
		return nil
	}
	return &ref
}

func pickOverrides(specific, fallback map[string]any) map[string]any {
	if specific != nil {
		return specific
	}
	return fallback
}
