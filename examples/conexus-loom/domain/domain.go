// Package domain holds the Conexus story model, ported to stand on its own. These
// types mirror the real Conexus world/narrative packages (World, Constitution,
// State, the Logician's structured output) trimmed to what this refactor
// exercises. They are plain data — persistence is loom's job.
package domain

import "github.com/google/uuid"

// World is the immutable, creator-authored template, produced by the migration
// pipeline from a block prompt (block.txt).
type World struct {
	ID         uuid.UUID   `json:"id"`
	Name       string      `json:"name"`
	Genre      string      `json:"genre"`
	Premise    string      `json:"premise"`
	Setting    string      `json:"setting"`
	Exposition string      `json:"exposition"`
	Characters []Character `json:"characters"`
	Locations  []Location  `json:"locations"`
}

// Character is a creator-defined character template.
type Character struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Physicality   string `json:"physicality,omitempty"`
	Psychology    string `json:"psychology,omitempty"`
	IsProtagonist bool   `json:"is_protagonist"`
}

// Location is a creator-defined place.
type Location struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Atmosphere  string `json:"atmosphere,omitempty"`
}

// Constitution carries the immutable laws, tone, and directives the agents must
// respect. It maps to per-story prompt-template variables.
type Constitution struct {
	ToneTags     []string        `json:"tone_tags"`
	WritingStyle string          `json:"writing_style"`
	VisualStyle  string          `json:"visual_style,omitempty"`
	POV          string          `json:"pov,omitempty"`
	Tense        string          `json:"tense,omitempty"`
	Difficulty   string          `json:"difficulty,omitempty"`
	Directives   StoryDirectives `json:"directives"`
}

// StoryDirectives are the creator-encoded rules of the playthrough.
type StoryDirectives struct {
	WinScenarios  []string `json:"win_scenarios"`
	LoseScenarios []string `json:"lose_scenarios"`
	KeyEvents     []string `json:"key_events"`
	TargetSteps   [2]int   `json:"target_steps"` // [min, max]
}

// PlayState is the mutable per-playthrough state (Conexus NarrativeState +
// GameState), stored in the loom Session's State.Vars.
type PlayState struct {
	Location          string   `json:"location"`
	Time              string   `json:"time"`
	CharactersPresent []string `json:"characters_present"`
	Facts             []string `json:"facts"`
	Tension           float64  `json:"tension"`
	Tone              string   `json:"tone"`
	Step              int      `json:"step"`
	PreviousOptions   []Option `json:"previous_options"`
	Status            string   `json:"status"` // ONGOING | ENDED_VICTORY | ENDED_DEFEAT | ENDED_NEUTRAL
}

// Option is one player choice produced by the Logician.
type Option struct {
	Label  string `json:"label"`
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

// LogicianOutput is the structured analysis the Logician returns for a turn,
// parsed from the model's JSON (mirrors Conexus's parse-and-validate approach).
type LogicianOutput struct {
	Status     string     `json:"status"`
	Options    []Option   `json:"options"`
	StatePatch StatePatch `json:"state_patch"`
	Reasoning  string     `json:"reasoning,omitempty"`
}

// StatePatch is the delta the Logician applies to PlayState.
type StatePatch struct {
	Location         string   `json:"location,omitempty"`
	Time             string   `json:"time,omitempty"`
	CharactersJoined []string `json:"characters_joined,omitempty"`
	CharactersLeft   []string `json:"characters_left,omitempty"`
	FactsAdd         []string `json:"facts_add,omitempty"`
	Tension          *float64 `json:"tension,omitempty"`
	Tone             string   `json:"tone,omitempty"`
}

// SensoryOutput is the art/sound direction for a turn.
type SensoryOutput struct {
	ImagePrompt string  `json:"image_prompt"`
	StyleLock   string  `json:"style_lock"`
	MusicMood   string  `json:"music_mood"`
	Intensity   float64 `json:"intensity"`
}

// Protagonist returns the protagonist's name, or "the protagonist".
func (w World) Protagonist() string {
	for _, c := range w.Characters {
		if c.IsProtagonist {
			return c.Name
		}
	}
	if len(w.Characters) > 0 {
		return w.Characters[0].Name
	}
	return "the protagonist"
}
