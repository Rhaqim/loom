package evaluator

import (
	"math"
	"strings"
	"testing"
)

func TestQuestionValidate(t *testing.T) {
	tests := []struct {
		name    string
		q       Question
		wantErr string
	}{
		{"noul ok", Noul("Is it urgent?"), ""},
		{"noul with criteria ok", NoulWith("Is it urgent?", "yes means", "no means"), ""},
		{"noul bad criteria", Question{Type: TypeNoul, Instructions: "q", Criteria: []any{"a"}}, "true\"/\"false"},
		{"choice ok", Choice("Which team?", map[string]any{"a": "A", "b": "B"}), ""},
		{"choice one option", Choice("Which?", map[string]any{"a": "A"}), "at least 2 options"},
		{"choice wrong criteria", Question{Type: TypeChoice, Instructions: "q", Criteria: []any{"a", "b"}}, "map of option"},
		{"score ok", Score("Rate it", "low", "high"), ""},
		{"score one level", Score("Rate it", "only"), "at least 2 levels"},
		{"score wrong criteria", Question{Type: TypeScore, Instructions: "q", Criteria: "low,high"}, "ordered slice"},
		{"no type", Question{Instructions: "q"}, "no type"},
		{"unknown type", Question{Type: "vibes", Instructions: "q"}, "unknown question type"},
		{"no instructions", Question{Type: TypeNoul}, "no instructions"},
		{"empty instructions", Noul(""), "empty instructions"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.q.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestChoiceOfBuildsBareOptions(t *testing.T) {
	q := ChoiceOf("Which?", "a", "b", "c")
	if err := q.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	crit := q.Criteria.(map[string]any)
	if len(crit) != 3 {
		t.Fatalf("got %d options, want 3", len(crit))
	}
	if v, ok := crit["a"]; !ok || v != nil {
		t.Fatalf("option a = %v (present %v), want a present nil description", v, ok)
	}
}

func TestValidateQuestionsNamesTheOffender(t *testing.T) {
	err := ValidateQuestions(map[string]Question{
		"good": Noul("fine"),
		"bad":  Score("rate", "only-one"),
	})
	if err == nil || !strings.Contains(err.Error(), `question "bad"`) {
		t.Fatalf("ValidateQuestions() = %v, want an error naming \"bad\"", err)
	}
}

func TestValidateQuestionsRejectsEmptySet(t *testing.T) {
	if err := ValidateQuestions(nil); err == nil {
		t.Fatal("ValidateQuestions(nil) = nil, want an error")
	}
}

func TestAnswerNoulAccessors(t *testing.T) {
	a := Answer{Type: TypeNoul, Noul: 0.95}
	if !a.Yes(0.8) {
		t.Error("Yes(0.8) = false, want true")
	}
	if a.No(0.2) {
		t.Error("No(0.2) = true, want false")
	}
	// A Noul reports its own certainty: 0.95 is 0.9 of the way to certain.
	if got := a.Certainty(); math.Abs(got-0.9) > 1e-9 {
		t.Errorf("Certainty() = %v, want 0.9", got)
	}
	// A coin flip must read as maximally uncertain, not as a weak yes.
	if got := (Answer{Type: TypeNoul, Noul: 0.5}).Certainty(); got != 0 {
		t.Errorf("Certainty() at p=0.5 = %v, want 0", got)
	}
}

func TestAnswerScoreLevelRounds(t *testing.T) {
	a := Answer{Type: TypeScore, Score: 1.43, Confidence: 0.7}
	if a.Level() != 1 {
		t.Errorf("Level() = %d, want 1", a.Level())
	}
	if a.Certainty() != 0.7 {
		t.Errorf("Certainty() = %v, want the Confidence 0.7", a.Certainty())
	}
	if !a.Certain(0.7) || a.Certain(0.71) {
		t.Error("Certain() should be inclusive at the threshold and false above it")
	}
}

func TestAnswerRankedIsDeterministic(t *testing.T) {
	a := Answer{
		Type:          TypeChoice,
		Choice:        "billing",
		Probabilities: map[string]float64{"billing": 0.5, "sales": 0.25, "technical": 0.25},
	}
	// Run repeatedly: Go randomises map iteration, so a single pass would not
	// catch a tie broken by iteration order.
	for range 20 {
		got := a.Ranked()
		if len(got) != 3 {
			t.Fatalf("Ranked() len = %d, want 3", len(got))
		}
		if got[0].Key != "billing" {
			t.Fatalf("Ranked()[0] = %q, want billing", got[0].Key)
		}
		// Equal probabilities must break by key, so the order never flips.
		if got[1].Key != "sales" || got[2].Key != "technical" {
			t.Fatalf("Ranked() tie order = %q,%q, want sales,technical", got[1].Key, got[2].Key)
		}
	}
}

func TestAnswersAccessorsAreMissSafe(t *testing.T) {
	// A gate written against a question that was never answered must fail open
	// rather than panic mid-turn — that is the whole point of the map type.
	var a Answers
	if a.Has("nope") {
		t.Error("Has() on a nil map = true, want false")
	}
	if a.Yes("nope", 0.5) {
		t.Error("Yes() on a missing answer = true, want false")
	}
	if a.Choice("nope") != "" || a.Score("nope") != 0 || a.Noul("nope") != 0 || a.Certainty("nope") != 0 {
		t.Error("accessors on a missing answer should read as zero values")
	}
	if got := a.Get("nope"); got.Type != "" || got.Probabilities != nil || got.Legend != nil {
		t.Errorf("Get() on a missing answer = %+v, want the zero Answer", got)
	}
}
