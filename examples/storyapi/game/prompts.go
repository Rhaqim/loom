package game

import "fmt"

// genres supported by the story engine.
var genres = map[string]genreDef{
	"fantasy": {
		label: "High Fantasy",
		tone:  "Classic high fantasy — ancient magic, noble quests, and epic stakes. Dungeons teeming with treasure and peril, cities alive with political intrigue, gods who occasionally meddle. The world rewards courage and cleverness.",
	},
	"sci-fi": {
		label: "Science Fiction",
		tone:  "Far-future science fiction. Starships, alien civilisations, synthetic intelligence, and the unresolved tension between technology and humanity. Hard science grounds the story; awe and existential wonder drive it.",
	},
	"horror": {
		label: "Horror",
		tone:  "Psychological horror. The threats are real, but dread is built through atmosphere and implication rather than gore. Isolation, paranoia, and the uncanny are your tools. Not every mystery should be solved.",
	},
	"mystery": {
		label: "Mystery",
		tone:  "Classic detective mystery. Clues are planted fairly; the reader can solve the case before the reveal. Every character has a secret. The truth is always more complicated than it appears.",
	},
	"thriller": {
		label: "Thriller",
		tone:  "High-stakes thriller. Every chapter ends on a knife-edge. Moral ambiguity, time pressure, and betrayal keep the tension relentless. The protagonist is always one step behind until they aren't.",
	},
	"romance": {
		label: "Romantic Drama",
		tone:  "Romantic drama with emotional depth. Focus on the interior lives of characters — their fears, desires, and the walls they've built. The external plot serves the internal journey.",
	},
	"historical": {
		label: "Historical Fiction",
		tone:  "Rigorously researched historical fiction. The period is a living, breathing world — not a museum exhibit. Characters are products of their time, with all its virtues and blind spots.",
	},
	"adventure": {
		label: "Adventure",
		tone:  "Swashbuckling adventure — shipwrecks, lost cities, ancient artefacts, and improbable escapes. Fast-paced and fun, with real consequences. The world is enormous and full of things waiting to be discovered.",
	},
}

type genreDef struct {
	label string
	tone  string
}

// GenreLabel returns the display name for a genre slug, or the slug itself if unknown.
func GenreLabel(genre string) string {
	if d, ok := genres[genre]; ok {
		return d.label
	}
	return genre
}

// ValidGenre returns true if genre is a supported slug.
func ValidGenre(genre string) bool {
	_, ok := genres[genre]
	return ok
}

// SystemPrompt returns the storyteller system prompt for the given genre.
func SystemPrompt(topicName, genre string) string {
	def, ok := genres[genre]
	if !ok {
		def = genres["fantasy"]
	}
	return fmt.Sprintf(`You are the narrator for an interactive story titled "%s".

TONE AND SETTING:
%s

YOUR ROLE:
- Narrate events with vivid sensory detail — what the protagonist sees, hears, smells, and feels.
- React honestly and consistently to the reader's choices; actions have consequences.
- Voice supporting characters with distinct personalities and motivations.
- End each response by creating a clear sense of "what happens next" or "what can you do" — without being mechanical about it.
- Keep responses to 2–4 paragraphs. Be vivid, not verbose.

RULES:
- Stay in narrative voice at all times. Do NOT break the fourth wall or reference the game/prompt system.
- Do NOT invent major plot events that contradict established facts in this session.
- If the reader's action is unclear or impossible within the story world, interpret it charitably or describe the attempt and its natural outcome.`, topicName, def.tone)
}

// UserTemplate returns the Go text/template used as the user-turn prompt.
// It injects the rolling conversation history stored in session.State.Vars
// so the narrator always has context for the full back-and-forth.
func UserTemplate() string {
	return `{{if index .Vars "history"}}Story so far:
{{index .Vars "history"}}
{{end}}Reader: {{.Action.Payload.text}}`
}
