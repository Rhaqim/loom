package narrative

import "testing"

func TestParseTitle(t *testing.T) {
	cases := map[string]string{
		"The Long Road Home":           "The Long Road Home",
		"  \n  The Long Road Home  \n": "The Long Road Home",
		"\"The Long Road Home\"":       "The Long Road Home",
		"```\nThe Long Road Home\n```": "The Long Road Home",
		"Title: The Long Road Home":    "Title: The Long Road Home",
		"":                             "",
		"   ":                          "",
		"one two three four five six seven eight nine ten eleven twelve thirteen": "one two three four five six seven eight nine ten eleven twelve",
		// JSON-shaped output yields "" so the engine falls back to the Logician title.
		"```json\n{\"title\":\"Road\"}\n```": "",
		"{\"title\":\"Road\"}":               "",
	}
	for in, want := range cases {
		if got := ParseTitle(in); got != want {
			t.Errorf("ParseTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSensoryV2AllChannels(t *testing.T) {
	const raw = `{
		"turn_id": "t1",
		"image": {"prompt":"a dark forest","style_lock":"noir","negative_prompt":"blurry",
			"characters_visible":[{"name":"Kade","appearance":"cloaked","action":"running","expression":"fearful"}]},
		"music": {"mood":"tense","intensity":0.7,"transition":"crossfade"},
		"sfx_cue":"a branch snaps","sfx_trigger_phrase":"snap","sfx_trigger_word_index":3,"sfx_gain":0.8,
		"voice": {"emotion":"fearful","speed_modifier":1.1,"intensity":0.6,"emphasis_words":["run","now"]},
		"ambient": {"atmosphere":"fog","cinematography":"vignette_soft","one_shot":null,"intensity":0.5,"color_tint":null}
	}`
	s, err := ParseSensory(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Image.Prompt != "a dark forest" || s.Image.NegativePrompt != "blurry" {
		t.Fatalf("image = %+v", s.Image)
	}
	if len(s.Image.CharactersVisible) != 1 || s.Image.CharactersVisible[0].Name != "Kade" ||
		s.Image.CharactersVisible[0].Expression != "fearful" {
		t.Fatalf("characters_visible = %+v", s.Image.CharactersVisible)
	}
	if s.Music.Mood != "tense" || s.Music.Transition != "crossfade" || s.Music.Intensity != 0.7 {
		t.Fatalf("music = %+v", s.Music)
	}
	if s.SFX.Cue != "a branch snaps" || s.SFX.TriggerWordIndex != 3 || s.SFX.Gain != 0.8 {
		t.Fatalf("sfx = %+v", s.SFX)
	}
	if s.Voice.Emotion != "fearful" || s.Voice.SpeedModifier != 1.1 || len(s.Voice.EmphasisWords) != 2 {
		t.Fatalf("voice = %+v", s.Voice)
	}
	if s.Ambient.Atmosphere != "fog" || s.Ambient.Cinematography != "vignette_soft" ||
		s.Ambient.OneShot != "" || s.Ambient.ColorTint != "" || s.Ambient.Intensity != 0.5 {
		t.Fatalf("ambient = %+v", s.Ambient)
	}
	// flat mirrors stay populated for backward-compat
	if s.ImagePrompt != "a dark forest" || s.MusicMood != "tense" {
		t.Fatalf("flat mirrors not set: %+v", s)
	}
}

func TestParseSensoryFlatMirrorsIntoChannels(t *testing.T) {
	s, err := ParseSensory(`{"image_prompt":"a castle","style_lock":"epic","music_mood":"triumphant","intensity":0.9}`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Image.Prompt != "a castle" || s.Image.StyleLock != "epic" {
		t.Fatalf("flat image not mirrored: %+v", s.Image)
	}
	if s.Music.Mood != "triumphant" || s.Music.Intensity != 0.9 {
		t.Fatalf("flat music not mirrored: %+v", s.Music)
	}
}

func TestParseSensoryFlatMusicOnly(t *testing.T) {
	// A flat response with music but no image must still keep the music direction.
	s, err := ParseSensory(`{"music_mood":"calm","intensity":0.3}`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Music.Mood != "calm" || s.MusicMood != "calm" || s.Music.Intensity != 0.3 {
		t.Fatalf("music-only flat dropped the music direction: %+v", s)
	}
}

func TestFlowIncludesTitleFollower(t *testing.T) {
	versions := map[string][]int{
		AgentAuthor: {1}, AgentLogician: {1}, AgentSensory: {1}, AgentTitle: {1},
	}
	flow := Flow(versions, 1)

	if flow.Lead.AgentSlug != AgentAuthor {
		t.Fatalf("lead = %q, want author", flow.Lead.AgentSlug)
	}
	var found bool
	for _, f := range flow.Followers {
		if f.AgentSlug == AgentTitle {
			found = true
		}
	}
	if !found {
		t.Fatalf("flow followers %v do not include the title agent", flow.Followers)
	}
}
