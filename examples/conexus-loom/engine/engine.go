// Package engine is the Conexus orchestrator, rebuilt on loom. It mirrors the
// monorepo's StoryOrchestrator.PlayTurn pipeline — load, frame, recall, generate
// the multi-agent turn, validate, apply state, persist, enqueue media — but every
// generation/persistence concern is delegated to loom, and every monorepo service
// is reached through a port so the real ones plug in later.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/rhaqim/conexus-refactor/domain"
	"github.com/rhaqim/conexus-refactor/migration"
	"github.com/rhaqim/conexus-refactor/narrative"
	"github.com/rhaqim/conexus-refactor/ports"
	loom "github.com/rhaqim/loom"
)

// Config wires loom and the monorepo seams.
type Config struct {
	Loom         *loom.Engine
	Memory       ports.MemoryVault
	Media        ports.MultimediaQueue
	Plugins      ports.PluginRegistry
	Prefs        ports.AIPreferences
	Log          ports.Logger
	Versions     map[string][]int // agent -> available prompt versions
	StoryVersion int              // prompt version to play this story at
	// ValidateSchemas registers loom's schema-validation post-hook so agent JSON
	// is checked against the response_format stored on its prompt (auto-retry on
	// violation). Leave false for the offline stub, whose reduced output does not
	// satisfy the full v2 schemas; enable for real providers.
	ValidateSchemas bool
	// QualityEngine registers the pre-process quality post-hooks (Slop Bans and
	// the Staleness Detector) on the Author. Leave false for the offline stub
	// (its fixed prose would trip the bans); enable for real providers.
	QualityEngine bool
}

// Conexus is the application engine.
type Conexus struct {
	cfg  Config
	flow loom.Flow
}

// New constructs the engine and registers the playthrough hooks on the loom
// engine: the memory-recall pre-hook and Logician QA post-hook always, plus the
// schema-validation and quality-engine (slop bans + staleness) post-hooks when
// the corresponding Config flags are set.
func New(cfg Config) *Conexus {
	if cfg.StoryVersion == 0 {
		cfg.StoryVersion = narrative.LatestVersion(cfg.Versions, narrative.AgentAuthor)
	}
	c := &Conexus{cfg: cfg, flow: narrative.Flow(cfg.Versions, cfg.StoryVersion)}
	cfg.Loom.Hooks().RegisterPre("memory-recall", c.memoryRecallHook())
	if cfg.ValidateSchemas {
		// Structural check first (against the prompt's stored schema), then the
		// semantic Logician QA (option count / verb-bucket dedupe).
		cfg.Loom.Hooks().RegisterPost("schema-validate", loom.SchemaValidationPostHook())
	}
	cfg.Loom.Hooks().RegisterPost("logician-qa", narrative.QAHook())
	if cfg.QualityEngine {
		// Author-side quality gates: reject cliché prose and scenes that repeat
		// the previous turn, asking loom to retry with guidance.
		cfg.Loom.Hooks().RegisterPost("slop-bans", narrative.SlopBanHook())
		cfg.Loom.Hooks().RegisterPost("staleness", narrative.StalenessHook(0))
	}
	return c
}

// StartStory migrates a block prompt into a world and opens a playthrough.
func (c *Conexus) StartStory(ctx context.Context, blockText, accountID string) (uuid.UUID, domain.World, error) {
	mig := &migration.Service{
		Loom:    c.cfg.Loom,
		Version: narrative.LatestVersion(c.cfg.Versions, narrative.AgentMigrate),
		Log:     c.cfg.Log,
	}
	world, con, err := mig.Migrate(ctx, blockText)
	if err != nil {
		return uuid.Nil, domain.World{}, err
	}

	startLoc := "the opening"
	if len(world.Locations) > 0 {
		startLoc = world.Locations[0].Name
	}
	play := domain.PlayState{
		Location:          startLoc,
		Time:              "the present moment",
		CharactersPresent: []string{world.Protagonist()},
		Tension:           0.3,
		Tone:              firstOr(con.ToneTags, "anticipatory"),
		Step:              0,
		Status:            "ONGOING",
	}
	sess := &loom.Session{
		PlatformID: accountID,
		State: loom.State{
			Modality: loom.ModalityText,
			Vars: map[string]any{
				"world":        toMap(world),
				"constitution": toMap(con),
				"play":         toMap(play),
				"bible":        buildBible(world, con),
			},
		},
		Tags:     []string{"conexus", world.Genre, fmt.Sprintf("promptv%d", c.cfg.StoryVersion)},
		Metadata: map[string]any{"world_id": world.ID.String(), "prompt_version": c.cfg.StoryVersion},
	}
	if err := c.cfg.Loom.Sessions().Create(ctx, sess); err != nil {
		return uuid.Nil, domain.World{}, err
	}
	// Seed memory with the cast and premise.
	_ = c.cfg.Memory.Store(ctx, sess.ID, "Premise: "+world.Premise)
	for _, ch := range world.Characters {
		_ = c.cfg.Memory.Store(ctx, sess.ID, fmt.Sprintf("%s: %s", ch.Name, ch.Description))
	}
	return sess.ID, world, nil
}

// TurnResult is the app-facing outcome of one turn.
type TurnResult struct {
	TurnID    string
	Title     string
	Prose     string
	Options   []domain.Option
	Sensory   *domain.SensoryOutput
	Status    string
	Step      int
	QARetried bool
	TokensIn  int
	TokensOut int
	USD       float64
}

// PlayTurn advances the story by one turn.
func (c *Conexus) PlayTurn(ctx context.Context, storyID uuid.UUID, accountID, action string, onChunk func(string)) (*TurnResult, error) {
	sess, err := c.cfg.Loom.Sessions().Get(ctx, storyID)
	if err != nil {
		return nil, fmt.Errorf("load story: %w", err)
	}
	var play domain.PlayState
	if err := fromMap(varsMap(sess, "play"), &play); err != nil {
		return nil, fmt.Errorf("decode play state: %w", err)
	}
	if play.Status != "ONGOING" {
		return nil, fmt.Errorf("story already %s", play.Status)
	}

	var world domain.World
	_ = fromMap(varsMap(sess, "world"), &world)
	var con domain.Constitution
	_ = fromMap(varsMap(sess, "constitution"), &con)
	protagonist := world.Protagonist()

	// Plugin pre-process (action gating / rewrite).
	injected := ""
	if plug, ok := c.cfg.Plugins.For(storyID); ok && action != "" {
		pr, perr := plug.PreProcess(ctx, &play, ports.PlayerContext{CharacterName: protagonist}, action)
		if perr != nil {
			return nil, perr
		}
		if !pr.Allow {
			return nil, fmt.Errorf("action blocked: %s", pr.Reason)
		}
		if pr.RewrittenText != "" {
			action = pr.RewrittenText
		}
		injected = pr.InjectContext
	}

	inputs := map[string]any{
		"action":       narrative.FrameAction(action, protagonist),
		"location":     play.Location,
		"visual_style": con.VisualStyle,
		"recent_prose": str(sess.State.Vars["last_prose"]),
		"plugin_note":  injected,
	}

	var sb strings.Builder
	turn, err := c.cfg.Loom.RunTurn(ctx, sess, loom.TurnRequest{
		Flow:   c.flow,
		Action: freeText(action),
		Inputs: inputs,
		// Turn-wide params (merged under each agent's own temperature); the
		// current narrative tension travels to every agent.
		Params: map[string]any{"tension": play.Tension},
		OnChunk: func(ch loom.Chunk) {
			sb.WriteString(ch.Content)
			if onChunk != nil {
				onChunk(ch.Content)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("run turn: %w", err)
	}

	res := &TurnResult{TurnID: turn.ID.String()}
	res.Prose = strings.TrimSpace(sb.String())
	if res.Prose == "" {
		res.Prose = loom.ResultText(turn.Lead.Result)
	}

	// Parse the Logician follower (QA hook already validated it).
	var logic domain.LogicianOutput
	if ls := turn.Followers[narrative.AgentLogician]; ls != nil {
		logic, _ = narrative.ParseLogician(narrative.JSONOf(ls.Result))
		for k := range ls.Diagnostics {
			if strings.HasPrefix(k, "retry_") {
				res.QARetried = true
			}
		}
	}
	// Parse the Sensory follower.
	if ss := turn.Followers[narrative.AgentSensory]; ss != nil {
		if so, perr := narrative.ParseSensory(narrative.JSONOf(ss.Result)); perr == nil {
			res.Sensory = &so
		}
	}
	// Parse the Title follower; fall back to the Logician's emitted title when
	// the dedicated agent produced nothing.
	if ts := turn.Followers[narrative.AgentTitle]; ts != nil {
		res.Title = narrative.ParseTitle(loom.ResultText(ts.Result))
	}
	if res.Title == "" {
		res.Title = strings.TrimSpace(logic.Title)
	}

	// Per-turn spend, summed synchronously from this turn's steps (lead +
	// followers, including QA retries' final results).
	res.TokensIn, res.TokensOut, res.USD = c.turnCost(turn)

	// Plugin post-process (mutate logician output / enrich state).
	if plug, ok := c.cfg.Plugins.For(storyID); ok {
		if perr := plug.PostProcess(ctx, &play, &logic); perr != nil {
			c.cfg.Log.Error("plugin postprocess", "err", perr)
		}
	}

	// Apply state patch.
	applyPatch(&play, logic.StatePatch)
	play.Step++
	play.PreviousOptions = logic.Options
	play.Status = finalize(logic.Status, play, con)

	// Persist facts to memory for future recall.
	for _, f := range logic.StatePatch.FactsAdd {
		_ = c.cfg.Memory.Store(ctx, storyID, f)
	}

	// Enqueue async media for every channel the sensory director produced.
	c.enqueueMedia(ctx, storyID, turn.ID.String(), res.Prose, res.Sensory)

	// Save updated state.
	sess.State.Vars["play"] = toMap(play)
	sess.State.Vars["last_prose"] = res.Prose
	if err := c.cfg.Loom.Sessions().Update(ctx, sess); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	res.Options = logic.Options
	res.Status = play.Status
	res.Step = play.Step
	return res, nil
}

// enqueueMedia fans the sensory direction out to the media queue, one task per
// channel the turn produced. The provider behind the queue consumes Prompt (the
// primary text) plus the channel-specific Params; an empty channel is skipped.
func (c *Conexus) enqueueMedia(ctx context.Context, storyID uuid.UUID, turnID, prose string, s *domain.SensoryOutput) {
	if s == nil {
		return
	}
	enq := func(kind ports.MediaKind, prompt string, params map[string]any) {
		_ = c.cfg.Media.Enqueue(ctx, ports.MediaTask{
			StoryID: storyID, TurnID: turnID, Kind: kind, Prompt: prompt, Params: params,
		})
	}
	if s.Image.Prompt != "" {
		enq(ports.MediaImage, s.Image.Prompt, map[string]any{
			"style_lock":         s.Image.StyleLock,
			"negative_prompt":    s.Image.NegativePrompt,
			"characters_visible": s.Image.CharactersVisible,
		})
	}
	if s.Music.Mood != "" {
		enq(ports.MediaMusic, s.Music.Mood, map[string]any{
			"mood": s.Music.Mood, "intensity": s.Music.Intensity, "transition": s.Music.Transition,
		})
	}
	if s.SFX.Cue != "" {
		enq(ports.MediaSFX, s.SFX.Cue, map[string]any{
			"trigger_phrase": s.SFX.TriggerPhrase, "trigger_word_index": s.SFX.TriggerWordIndex, "gain": s.SFX.Gain,
		})
	}
	// Voice narrates the turn's prose with the director's emotion/speed.
	if prose != "" {
		enq(ports.MediaVoice, prose, map[string]any{
			"emotion": s.Voice.Emotion, "speed_modifier": s.Voice.SpeedModifier,
			"intensity": s.Voice.Intensity, "emphasis_words": s.Voice.EmphasisWords,
		})
	}
	if s.Ambient.Atmosphere != "" || s.Ambient.Cinematography != "" || s.Ambient.OneShot != "" || s.Ambient.ColorTint != "" {
		prompt := firstNonEmpty(s.Ambient.Atmosphere, s.Ambient.Cinematography, s.Ambient.OneShot, s.Ambient.ColorTint)
		enq(ports.MediaAmbient, prompt, map[string]any{
			"atmosphere": s.Ambient.Atmosphere, "cinematography": s.Ambient.Cinematography,
			"one_shot": s.Ambient.OneShot, "intensity": s.Ambient.Intensity, "color_tint": s.Ambient.ColorTint,
		})
	}
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Fork branches the story at the current head for an alternate timeline.
func (c *Conexus) Fork(ctx context.Context, storyID uuid.UUID) (uuid.UUID, error) {
	parent, err := c.cfg.Loom.Sessions().Get(ctx, storyID)
	if err != nil {
		return uuid.Nil, err
	}
	branch, err := c.cfg.Loom.Sessions().Fork(ctx, storyID, len(parent.History))
	if err != nil {
		return uuid.Nil, err
	}
	return branch.ID, nil
}

// Snapshot is the persisted play state needed to resume a story.
type Snapshot struct {
	Step    int
	Status  string
	Options []domain.Option
}

// World returns the migrated world for a story (for display / resume headers).
func (c *Conexus) World(ctx context.Context, storyID uuid.UUID) (domain.World, error) {
	sess, err := c.cfg.Loom.Sessions().Get(ctx, storyID)
	if err != nil {
		return domain.World{}, err
	}
	var w domain.World
	err = fromMap(varsMap(sess, "world"), &w)
	return w, err
}

// SessionUSD returns loom's recorded total spend for a story (cross-check of the
// per-turn figures; populated as async cost records land).
func (c *Conexus) SessionUSD(ctx context.Context, storyID uuid.UUID) float64 {
	u, err := c.cfg.Loom.Cost().SessionUsage(ctx, storyID)
	if err != nil || u == nil {
		return 0
	}
	return u.TotalUSD
}

// Snapshot returns the current step, status, and pending options for a story.
func (c *Conexus) Snapshot(ctx context.Context, storyID uuid.UUID) (*Snapshot, error) {
	sess, err := c.cfg.Loom.Sessions().Get(ctx, storyID)
	if err != nil {
		return nil, err
	}
	var play domain.PlayState
	if err := fromMap(varsMap(sess, "play"), &play); err != nil {
		return nil, err
	}
	return &Snapshot{Step: play.Step, Status: play.Status, Options: play.PreviousOptions}, nil
}

// memoryRecallHook injects recalled facts into the turn before the prompt renders.
func (c *Conexus) memoryRecallHook() loom.PreHook {
	return func(ctx context.Context, req *loom.StepRequest) error {
		if req.Session == nil || c.cfg.Memory == nil {
			return nil
		}
		query := str(req.Inputs["action"])
		if query == "" {
			return nil
		}
		facts, err := c.cfg.Memory.Recall(ctx, req.Session.ID, query, 4)
		if err != nil || len(facts) == 0 {
			return nil
		}
		if req.Inputs == nil {
			req.Inputs = map[string]any{}
		}
		req.Inputs["recalled_memories"] = facts
		return nil
	}
}

// ---- helpers ----

// turnCost sums tokens and USD across every step in a turn, pricing each step by
// the model the generator reported (loom.Config.Pricing), so a turn that mixes
// models is costed correctly.
func (c *Conexus) turnCost(turn *loom.Turn) (in, out int, usd float64) {
	for _, s := range turn.Steps {
		i, o := tokensOf(s.Result)
		in += i
		out += o
		usd += c.cfg.Loom.PriceResult(loom.ResultModel(s.Result), i, o)
	}
	return
}

func tokensOf(res loom.Result) (int, int) {
	switch r := res.(type) {
	case *loom.TextResult:
		return r.InputTokens, r.OutputTokens
	case *loom.StructuredResult:
		return r.InputTokens, r.OutputTokens
	}
	return 0, 0
}

func applyPatch(p *domain.PlayState, patch domain.StatePatch) {
	if patch.Location != "" {
		p.Location = patch.Location
	}
	if patch.Time != "" {
		p.Time = patch.Time
	}
	if patch.Tone != "" {
		p.Tone = patch.Tone
	}
	if patch.Tension != nil {
		p.Tension = *patch.Tension
	}
	p.Facts = append(p.Facts, patch.FactsAdd...)
	p.CharactersPresent = append(p.CharactersPresent, patch.CharactersJoined...)
	if len(patch.CharactersLeft) > 0 {
		p.CharactersPresent = removeAll(p.CharactersPresent, patch.CharactersLeft)
	}
}

func finalize(status string, p domain.PlayState, con domain.Constitution) string {
	if status != "" && status != "ONGOING" {
		return status
	}
	if max := con.Directives.TargetSteps[1]; max > 0 && p.Step >= max {
		return "ENDED_NEUTRAL"
	}
	return "ONGOING"
}

func buildBible(w domain.World, c domain.Constitution) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TITLE: %s\nGENRE: %s\nPREMISE: %s\nSETTING: %s\n", w.Name, w.Genre, w.Premise, w.Setting)
	if w.Exposition != "" {
		fmt.Fprintf(&b, "EXPOSITION: %s\n", w.Exposition)
	}
	fmt.Fprintf(&b, "POV: %s  TENSE: %s  STYLE: %s\n", c.POV, c.Tense, c.WritingStyle)
	if len(c.ToneTags) > 0 {
		fmt.Fprintf(&b, "TONE: %s\n", strings.Join(c.ToneTags, ", "))
	}
	b.WriteString("CHARACTERS:\n")
	for _, ch := range w.Characters {
		star := ""
		if ch.IsProtagonist {
			star = " (PROTAGONIST)"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", ch.Name, star, ch.Description)
	}
	return b.String()
}

func removeAll(xs, drop []string) []string {
	d := map[string]bool{}
	for _, x := range drop {
		d[x] = true
	}
	out := xs[:0]
	for _, x := range xs {
		if !d[x] {
			out = append(out, x)
		}
	}
	return out
}

func firstOr(xs []string, fallback string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return fallback
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func fromMap(m map[string]any, out any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func varsMap(sess *loom.Session, key string) map[string]any {
	if m, ok := sess.State.Vars[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func freeText(action string) *loom.Action {
	if action == "" {
		return nil
	}
	return &loom.Action{Kind: loom.ActionFreeText, Payload: map[string]any{"text": action}}
}
