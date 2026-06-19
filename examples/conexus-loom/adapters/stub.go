package adapters

import (
	"context"
	"fmt"
	"strings"

	loom "github.com/rhaqim/loom"
)

// StubGenerator is an offline stand-in for a real text model. It inspects the
// system prompt to tell which agent is calling and returns valid canned output,
// so the full pipeline (migration → turn → QA → state) runs with no API key.
// Set CONEXUS_PROVIDER=stub to use it. Swap for generator/openai to go live.
type StubGenerator struct{}

func (StubGenerator) Modality() loom.Modality { return loom.ModalityText }

func (g StubGenerator) Generate(_ context.Context, req loom.GenerateRequest) (loom.Result, error) {
	content := g.respond(req)
	return loom.NewTextResult(content, "stop", len(req.UserPrompt)/4, len(content)/4), nil
}

func (g StubGenerator) GenerateStream(_ context.Context, req loom.GenerateRequest) (<-chan loom.Chunk, <-chan loom.Result, error) {
	content := g.respond(req)
	chunks := make(chan loom.Chunk, 32)
	results := make(chan loom.Result, 1)
	go func() {
		defer close(chunks)
		defer close(results)
		for _, w := range strings.Fields(content) {
			chunks <- loom.Chunk{Content: w + " "}
		}
		results <- loom.NewTextResult(content, "stop", len(req.UserPrompt)/4, len(content)/4)
	}()
	return chunks, results, nil
}

func (g StubGenerator) respond(req loom.GenerateRequest) string {
	sys := req.SystemPrompt
	switch {
	case strings.Contains(sys, "convert a creator's block prompt"):
		return stubMigration
	// Matches both the v1 prompts ("You are the Logician") and the real conexus
	// v2 prompts ("...produce structured metadata...").
	case strings.Contains(sys, "You are the Logician") || strings.Contains(sys, "produce structured metadata"):
		return g.stubLogician(req)
	case strings.Contains(sys, "You are the Sensory Director") || strings.Contains(sys, "produce media cues"):
		return stubSensory
	default: // Author
		return g.stubAuthor(req)
	}
}

func (StubGenerator) stubAuthor(req loom.GenerateRequest) string {
	action := ""
	if a, ok := req.Inputs["action"].(string); ok {
		action = a
	}
	if action == "" {
		return "The floodlights buzzed over an empty pitch while, beyond the stands, the city held its breath. He laced his boots one eyelet at a time, listening to a chant rise somewhere past the gates — half song, half threat."
	}
	return fmt.Sprintf("He weighed it for a long moment. %s. The decision settled in his chest like a stone, and the noise of the crowd seemed to fall away, leaving only the next step to take.", strings.TrimSuffix(action, "."))
}

func (StubGenerator) stubLogician(req loom.GenerateRequest) string {
	// Diversify only after a QA retry, to demonstrate the hook→retry loop.
	if len(req.Annotations) == 0 {
		return `{"status":"ONGOING","reasoning":"stub","options":[
			{"label":"Confront the ultras","type":"bold","detail":"Face them down."},
			{"label":"Confront the official","type":"bold","detail":"Demand the truth."},
			{"label":"Confront your brother","type":"emotional","detail":"Have it out."}],
			"state_patch":{"facts_add":["Tension is rising in the dressing room."],"tension":0.5,"tone":"tense"}}`
	}
	return `{"status":"ONGOING","reasoning":"stub diversified","options":[
		{"label":"Confront the ultras","type":"bold","detail":"Face them down."},
		{"label":"Slip away from the stadium","type":"cautious","detail":"Avoid the mob."},
		{"label":"Call Ana for counsel","type":"social","detail":"Seek a steady voice."}],
		"state_patch":{"facts_add":["Tension is rising in the dressing room."],"tension":0.5,"tone":"tense"}}`
}

const stubSensory = `{"image_prompt":"a young footballer in a darkened tunnel, floodlights behind, tense crowd, gritty 1990s film grain","style_lock":"desaturated 1990s photojournalism","music_mood":"tense","intensity":0.6}`

const stubMigration = `{
  "world": {
    "name": "Last Derby",
    "genre": "historical drama",
    "premise": "A young Red Star Belgrade midfielder must choose his path as Yugoslavia collapses.",
    "setting": "Early 1990s Belgrade, a nation splitting apart through its football.",
    "exposition": "Red Star has just won the European Cup as war brews and the stands radicalize.",
    "characters": [
      {"name":"Milos Petrovic","description":"Star Serbian midfielder torn between career, war, and pride.","is_protagonist":true},
      {"name":"Dragan Stojanovic","description":"Red Star captain and mentor trying to hold the team together.","is_protagonist":false},
      {"name":"Marko Radic","description":"Radical ultra leader who expects Milos to be a national warrior.","is_protagonist":false},
      {"name":"Ana Markovic","description":"Milos's girlfriend, a law student against the war.","is_protagonist":false}
    ],
    "locations": [
      {"name":"The Marakana dressing room","description":"Red Star's locker room, fracturing along ethnic lines.","atmosphere":"charged, claustrophobic"}
    ]
  },
  "constitution": {
    "tone_tags": ["serious","dramatic","historical"],
    "writing_style": "narrative, past tense, active voice",
    "visual_style": "desaturated 1990s photojournalism",
    "pov": "third-person",
    "tense": "past",
    "difficulty": "hard",
    "directives": {
      "win_scenarios": ["Becomes an international legend abroad","Stays as a national hero","Keeps his teammates united as the peacemaker"],
      "lose_scenarios": ["Killed in the war","Banned from football","Exiled by the ultras as a traitor"],
      "key_events": ["Dressing-room tensions boil over","Officials reveal a foreign transfer offer","Draft papers arrive for friends","Violence erupts at the Dinamo Zagreb match"],
      "target_steps": [8, 14]
    }
  }
}`
