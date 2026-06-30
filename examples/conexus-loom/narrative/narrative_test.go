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
