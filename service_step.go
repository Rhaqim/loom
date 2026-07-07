package loom

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type stepService struct {
	e          *Engine
	maxRetries int
}

func (s *stepService) run(ctx context.Context, session *Session, req StepRequest) (*Step, error) {
	start := time.Now()

	// Resolve agent.
	agent := req.Agent
	if agent == nil {
		var err error
		agent, err = s.e.agents.Get(ctx, req.AgentSlug, req.AgentVersion)
		if err != nil {
			return nil, fmt.Errorf("loom: resolve agent %q: %w", req.AgentSlug, err)
		}
	}

	// Enforce budgets before the generator is chosen: an exceeded budget may
	// block this step (BudgetExceededError) or downgrade it to the agent's
	// fallback, so it must run before generator resolution.
	if downgraded, err := s.e.budgets.enforce(ctx, session, agent); err != nil {
		return nil, err
	} else {
		agent = downgraded
	}

	// Look up generator. A per-request GeneratorOverride wins over the agent's
	// configured generator, enabling per-call provider/model routing.
	genSlug := agent.GeneratorSlug
	if req.GeneratorOverride != "" {
		genSlug = req.GeneratorOverride
	}
	gen, ok := s.e.generator(genSlug)
	if !ok {
		return nil, fmt.Errorf("loom: generator %q not registered", genSlug)
	}

	// Each step runs against a per-step copy of the session so concurrent
	// followers in a turn mutate/read their own State.Vars instead of racing on
	// the shared map; hook-written Vars are merged back under the lock below. The
	// copy is taken under the read lock so it does not race a sibling follower's
	// locked persist tail (which appends History and writes State).
	s.e.mu.RLock()
	stepSession := session.forStep()
	s.e.mu.RUnlock()

	// Expose the (per-step) session to hooks before they run.
	req.Session = stepSession

	// Run pre-hooks first so they can mutate session state (e.g. inject recalled
	// memories into State.Vars or set req.Inputs) *before* the user template is
	// rendered from that state.
	if err := s.e.hooks.RunPre(ctx, &req); err != nil {
		if IsSkip(err) {
			return nil, err // propagate skip to caller
		}
		return nil, fmt.Errorf("loom: pre-hook: %w", err)
	}

	// Build GenerateRequest (renders user template, loads system prompt) using
	// the (possibly hook-mutated) session and request.
	genReq, err := s.buildGenerateRequest(ctx, stepSession, agent, &req)
	if err != nil {
		return nil, err
	}
	// Surface the resolved output contract so post-hooks can validate against it.
	req.ResponseFormat = genReq.ResponseFormat

	// Generation loop with retry support.
	var result Result
	diagnostics := map[string]any{}
	// The directive-free system prompt, so retry hints are re-applied from a
	// clean base each attempt rather than stacking across attempts.
	baseSystemPrompt := genReq.SystemPrompt

	// A per-request cap overrides the engine default (0 = use the default).
	maxRetries := s.maxRetries
	if req.MaxRetries > 0 {
		maxRetries = req.MaxRetries
	}
	// Under RetryKeepBest the engine scores every attempt and persists the
	// lowest-scoring draft rather than failing on exhaustion; retries run
	// silently (sync, no chunks) so only attempt 0 reaches the caller's OnChunk.
	keepBest := req.RetryMode == RetryKeepBest
	var best Result
	var bestScore int
	var haveBest bool
	if keepBest {
		diagnostics["retry_mode"] = "keep_best"
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Carry the accumulated retry hints on the request and fold them into
			// the prompt itself, so any generator honors them — even ones that
			// ignore GenerateRequest.Annotations.
			genReq.Annotations = req.annotations
			genReq.SystemPrompt = withRetryDirective(baseSystemPrompt, req.annotations)
		}

		// Stream attempt 0; keep-best silences retries so a second draft never
		// leaks to the caller's chunk sink.
		onChunk := req.OnChunk
		if keepBest && attempt > 0 {
			onChunk = nil
		}

		// Streaming vs sync generation.
		if sg, ok := gen.(StreamingGenerator); ok && onChunk != nil {
			chunks, resultCh, streamErr := sg.GenerateStream(ctx, genReq)
			if streamErr != nil {
				if keepBest && haveBest {
					diagnostics[fmt.Sprintf("attempt_%d_error", attempt)] = streamErr.Error()
					break
				}
				return nil, fmt.Errorf("loom: generator stream: %w", streamErr)
			}
			// Forward chunks until the adapter closes the channel, honouring
			// cancellation so a stuck stream cannot hang the step.
		drainChunks:
			for {
				select {
				case c, more := <-chunks:
					if !more {
						break drainChunks
					}
					onChunk(c)
				case <-ctx.Done():
					return nil, fmt.Errorf("loom: generator stream: %w", ctx.Err())
				}
			}
			var more bool
			select {
			case result, more = <-resultCh:
			case <-ctx.Done():
				return nil, fmt.Errorf("loom: generator stream: %w", ctx.Err())
			}
			// Mirror the sync error path: a closed channel (no result) or a failed
			// result is a generator error, not a successful empty turn.
			if !more || result == nil {
				if keepBest && haveBest {
					diagnostics[fmt.Sprintf("attempt_%d_error", attempt)] = "no result"
					break
				}
				return nil, fmt.Errorf("loom: generator stream: no result")
			}
			if result.Status() == ResultStatusFailed {
				msg, _ := result.Metadata()["error"].(string)
				if keepBest && haveBest {
					if msg == "" {
						msg = "failed"
					}
					diagnostics[fmt.Sprintf("attempt_%d_error", attempt)] = msg
					break
				}
				if msg != "" {
					return nil, fmt.Errorf("loom: generator stream: %s", msg)
				}
				return nil, fmt.Errorf("loom: generator stream: failed")
			}
			// Signal that the streamed draft is complete, before post-hooks run,
			// so the caller can close its chunk sink at this timing even if a
			// silent keep-best retry follows.
			if req.OnStreamEnd != nil {
				req.OnStreamEnd(attempt)
			}
		} else {
			result, err = gen.Generate(ctx, genReq)
			if err != nil {
				if keepBest && haveBest {
					diagnostics[fmt.Sprintf("attempt_%d_error", attempt)] = err.Error()
					break
				}
				return nil, fmt.Errorf("loom: generator: %w", err)
			}
		}

		// Run post-hooks. Expose the attempt index so a hook can self-cap retries.
		req.attempt = attempt
		var hookErr error
		result, hookErr = s.e.hooks.RunPost(ctx, &req, result)
		if hookErr == nil {
			if keepBest {
				best, bestScore, haveBest = result, 0, true
			}
			break // success
		}
		if !IsRetry(hookErr) {
			return nil, fmt.Errorf("loom: post-hook: %w", hookErr)
		}

		var rerr *RetryError
		errors.As(hookErr, &rerr)
		ann := rerr.Annotation
		diagnostics[fmt.Sprintf("retry_%d_reason", attempt+1)] = ann.Reason

		if keepBest {
			diagnostics[fmt.Sprintf("attempt_%d_score", attempt)] = rerr.Score
			// A non-positive score with a keepable draft is an honor-system
			// acceptance: keep it and stop.
			if rerr.Score <= 0 && rerr.Draft != nil {
				best, bestScore, haveBest = rerr.Draft, 0, true
				break
			}
			// Track the lowest-scoring draft; strict < keeps the earlier on a tie.
			if rerr.Draft != nil && (!haveBest || rerr.Score < bestScore) {
				best, bestScore, haveBest = rerr.Draft, rerr.Score, true
			}
			if attempt == maxRetries {
				break // exhaustion keeps the best draft rather than failing
			}
			req.annotations = append(req.annotations, ann)
			continue
		}

		// RetryDiscard: exhausting the budget fails the step.
		if attempt == maxRetries {
			return nil, fmt.Errorf("loom: max retries (%d) exceeded: %w", maxRetries, hookErr)
		}
		req.annotations = append(req.annotations, ann)
	}

	// Resolve the kept draft for keep-best. A run where no attempt ever produced
	// a keepable draft is the one keep-best failure mode.
	if keepBest {
		if !haveBest {
			return nil, fmt.Errorf("loom: keep-best produced no usable result after %d attempts", maxRetries+1)
		}
		result = best
		diagnostics["kept_score"] = bestScore
	}

	// Build and persist step.
	// Ensure the action has a unique ID before persisting.
	if req.Action != nil && req.Action.ID == (uuid.UUID{}) {
		req.Action.ID = uuid.New()
	}
	// Tag the step with turn linkage when it is part of a RunTurn flow so the
	// steps of one turn can be grouped for branching, replay, and cost rollups.
	if req.turnID != uuid.Nil {
		diagnostics["turn_id"] = req.turnID.String()
		diagnostics["turn_role"] = req.turnRole
	}

	// Serialise the session-mutating tail of the step. RunTurn runs follower
	// agents concurrently against the same *Session; without this lock their
	// index assignment and History appends would race.
	s.e.mu.Lock()
	defer s.e.mu.Unlock()

	step := &Step{
		ID:          uuid.New(),
		SessionID:   session.ID,
		Index:       len(session.History),
		AgentID:     agent.ID,
		Request:     buildStoredRequest(genReq),
		Result:      result,
		Action:      req.Action,
		Diagnostics: diagnostics,
		DurationMs:  int(time.Since(start).Milliseconds()),
		CreatedAt:   time.Now(),
	}
	// Finalize the post-step state before persisting so the step and its
	// checkpoint commit in one transaction. Reconcile any Vars the hooks wrote on
	// the per-step copy back onto the shared session (last writer wins).
	session.State.Modality = result.Modality()
	mergeVars(&session.State, stepSession.State.Vars)
	if err := insertStep(ctx, s.e.db, s.e.prefix, step, &session.State); err != nil {
		return nil, fmt.Errorf("loom: persist step: %w", err)
	}

	// Update session history and the session row.
	session.History = append(session.History, *step)
	if err := s.e.sessions.Update(ctx, session); err != nil {
		// A version conflict means another writer advanced the row (only possible
		// across engine instances). Resync our version so later steps aren't
		// permanently frozen; the contended state resolves last-writer-wins.
		// Full reconciliation is deferred with the multi-instance work.
		if errors.Is(err, ErrSessionConflict) {
			// Flag the losing step so callers (and metrics) can observe the
			// cross-instance contention rather than only seeing a log line.
			step.Diagnostics["session_conflict"] = true
			if fresh, gerr := querySession(ctx, s.e.db, s.e.prefix, session.ID); gerr == nil {
				session.Version = fresh.Version
			}
		}
		s.e.log.Error("update session after step", "session_id", session.ID, "err", err)
	}

	// Record cost asynchronously (best-effort).
	go s.e.costs.recordFromResult(context.Background(), step, agent, result, session.PlatformID)

	return step, nil
}

// resolveSystemPrompt loads a system-prompt override from the registry
// (Slug+Version) or a file (File). An empty ref yields "".
func (s *stepService) resolveSystemPrompt(ctx context.Context, ref PromptRef) (string, error) {
	if ref.Literal != "" {
		return ref.Literal, nil
	}
	if ref.File != "" {
		path, err := s.e.confinedPromptFile(ref.File)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if ref.Slug != "" {
		p, err := s.e.prompts.Get(ctx, ref.Slug, ref.Version)
		if err != nil {
			return "", err
		}
		return p.Body, nil
	}
	return "", nil
}

// confinedPromptFile validates a PromptRef.File path and returns the concrete
// path to read. File-backed prompts are disabled unless Config.PromptFileRoot is
// set; when set, the requested path (with symlinks evaluated) must lie inside
// that root. This prevents a caller-supplied path from reading arbitrary files
// (e.g. /etc/passwd, ~/.aws/credentials) whose contents would otherwise be sent
// to the LLM and persisted.
func (e *Engine) confinedPromptFile(file string) (string, error) {
	root := e.cfg.PromptFileRoot
	if root == "" {
		return "", fmt.Errorf("loom: file-backed prompts are disabled; set Config.PromptFileRoot to enable them")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("loom: prompt file root: %w", err)
	}
	// Resolve symlinks on the root so the containment check compares real paths.
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}

	target := file
	if !filepath.IsAbs(target) {
		target = filepath.Join(absRoot, target)
	}
	target = filepath.Clean(target)
	// Evaluate symlinks on the target when it exists so a symlink inside the root
	// cannot point outside it. A non-existent path (ENOENT) is left as-is; the
	// containment check below still applies and the subsequent read will fail.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}

	rootWithSep := absRoot + string(os.PathSeparator)
	if target != absRoot && !strings.HasPrefix(target, rootWithSep) {
		return "", fmt.Errorf("loom: prompt file %q is outside PromptFileRoot", file)
	}
	return target, nil
}

func (s *stepService) buildGenerateRequest(
	ctx context.Context,
	session *Session,
	agent *Agent,
	req *StepRequest,
) (GenerateRequest, error) {
	action := req.Action
	annotations := req.annotations
	// Load system prompt — cached by UUID.
	systemPrompt := ""
	if agent.SystemPromptID != uuid.Nil {
		spKey := cacheKey("prompt-id", s.e.prefix, agent.SystemPromptID.String(), 0)
		var sp *Prompt
		if cached, ok := cacheGet[Prompt](ctx, s.e.cache, spKey); ok {
			sp = cached
		} else {
			var err error
			sp, err = queryPromptByID(ctx, s.e.db, s.e.prefix, agent.SystemPromptID)
			if err != nil {
				return GenerateRequest{}, fmt.Errorf("load system prompt: %w", err)
			}
			cacheSet(ctx, s.e.cache, spKey, sp)
		}
		systemPrompt = sp.Body
	}
	// A per-call system-prompt override (e.g. a harness prompt variant) replaces
	// the agent's system prompt for this step only.
	if req.SystemPromptOverride != nil {
		body, err := s.resolveSystemPrompt(ctx, *req.SystemPromptOverride)
		if err != nil {
			return GenerateRequest{}, fmt.Errorf("resolve system-prompt override: %w", err)
		}
		if body != "" {
			systemPrompt = body
		}
	}

	// Load and render user template — cached by UUID.
	userPrompt := ""
	if agent.UserTemplateID != uuid.Nil {
		utKey := cacheKey("prompt-id", s.e.prefix, agent.UserTemplateID.String(), 0)
		var ut *Prompt
		if cached, ok := cacheGet[Prompt](ctx, s.e.cache, utKey); ok {
			ut = cached
		} else {
			var err error
			ut, err = queryPromptByID(ctx, s.e.db, s.e.prefix, agent.UserTemplateID)
			if err != nil {
				return GenerateRequest{}, fmt.Errorf("load user template: %w", err)
			}
			cacheSet(ctx, s.e.cache, utKey, ut)
		}
		// Render under a read lock — a sibling follower's locked tail may be
		// appending to session.History / writing session.State right now.
		var err error
		s.e.mu.RLock()
		userPrompt, err = renderTemplate(ut.Body, session, action, req.Inputs, req.Params)
		s.e.mu.RUnlock()
		if err != nil {
			return GenerateRequest{}, fmt.Errorf("render user template: %w", err)
		}
	}

	params := agent.Params
	if req.ParamOverride != nil {
		params = *req.ParamOverride
	}
	// params is a shallow struct copy, so params.Extra still aliases the agent's
	// (or override's) shared map. Clone it before applyParamsMap mutates it in
	// place — otherwise concurrent steps sharing the agent race/corrupt that map
	// and one turn's params leak into the next. Clone(nil) is nil (then allocated).
	params.Extra = maps.Clone(params.Extra)
	// Fold the free-form params map onto the typed params (known model keys) and
	// keep the whole map for the generator and template.
	applyParamsMap(&params, req.Params)

	// Resolve the response format — an independent piece of the agent's
	// composition (system prompt + user template + response format). A referenced
	// record (shared, reusable) wins over an inline value.
	responseFormat := agent.ResponseFormat
	if agent.ResponseFormatID != nil && *agent.ResponseFormatID != uuid.Nil {
		rec, err := s.e.responseFormats.GetByID(ctx, *agent.ResponseFormatID)
		if err != nil {
			return GenerateRequest{}, fmt.Errorf("load response format: %w", err)
		}
		responseFormat = rec.Format()
	}

	// Snapshot history under a read lock — RunTurn runs follower agents
	// concurrently, and a sibling's locked tail may be appending right now.
	s.e.mu.RLock()
	histContext := collectResultContext(session.History)
	s.e.mu.RUnlock()

	return GenerateRequest{
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Params:         params,
		ResponseFormat: responseFormat,
		Context:        histContext,
		SessionID:      session.ID,
		AgentID:        agent.ID,
		Annotations:    annotations,
		Inputs:         req.Inputs,
		Overrides:      req.Overrides,
		ParamsMap:      req.Params,
		Bus:            req.bus,
	}, nil
}

// withRetryDirective appends a system-prompt addendum describing why the prior
// attempt was rejected and what to avoid, so the model actually corrects on the
// next attempt. Generators also receive the hints on GenerateRequest.Annotations,
// but most ignore that field — folding them into the prompt makes retries work
// regardless of the generator. Returns systemPrompt unchanged when there is
// nothing to add.
func withRetryDirective(systemPrompt string, anns []RetryAnnotation) string {
	d := annotationDirective(anns)
	switch {
	case d == "":
		return systemPrompt
	case systemPrompt == "":
		return d
	default:
		return systemPrompt + "\n\n" + d
	}
}

// annotationDirective renders retry hints into one instruction block, de-duping
// reasons and forbidden phrases, or "" when there is nothing to say.
func annotationDirective(anns []RetryAnnotation) string {
	var reasons, forbidden []string
	seenReason, seenPhrase := map[string]bool{}, map[string]bool{}
	for _, a := range anns {
		if r := strings.TrimSpace(a.Reason); r != "" && !seenReason[r] {
			seenReason[r] = true
			reasons = append(reasons, r)
		}
		for _, f := range a.Forbidden {
			if f = strings.TrimSpace(f); f != "" && !seenPhrase[f] {
				seenPhrase[f] = true
				forbidden = append(forbidden, f)
			}
		}
	}
	if len(reasons) == 0 && len(forbidden) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Your previous attempt was rejected and must be corrected.")
	if len(reasons) > 0 {
		b.WriteString(" Reason: ")
		b.WriteString(strings.Join(reasons, "; "))
		b.WriteString(".")
	}
	if len(forbidden) > 0 {
		b.WriteString(" Do not use any of these words or phrases: ")
		b.WriteString(strings.Join(forbidden, ", "))
		b.WriteString(".")
	}
	return b.String()
}

// applyParamsMap folds known model keys from a free-form params map onto the
// typed GenerateParams, and accumulates every key (including domain knobs like
// "tension") into Params.Extra so generators can read them.
func applyParamsMap(p *GenerateParams, m map[string]any) {
	if len(m) == 0 {
		return
	}
	for k, v := range m {
		switch k {
		case "temperature":
			p.Temperature = toFloat(v)
		case "top_p":
			p.TopP = toFloat(v)
		case "frequency_penalty":
			p.FrequencyPenalty = toFloat(v)
		case "presence_penalty":
			p.PresencePenalty = toFloat(v)
		case "duration_sec":
			p.DurationSec = toFloat(v)
		case "max_tokens":
			p.MaxTokens = toInt(v)
		case "seed":
			p.Seed = toInt(v)
		case "width":
			p.Width = toInt(v)
		case "height":
			p.Height = toInt(v)
		}
	}
	if p.Extra == nil {
		p.Extra = map[string]any{}
	}
	maps.Copy(p.Extra, m)
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	}
	return 0
}

// templateSession is the minimal, whitelisted view of a Session exposed to user
// templates. It deliberately omits Session.Metadata (platform-controlled data
// that commonly holds secrets/tenant keys) and Session.History (every prior
// turn's request and result), both of which a template author could otherwise
// render into the prompt to exfiltrate secrets or cross-turn data via
// {{.Session.Metadata.x}} / {{range .Session.History}}.
