package narrative

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/evaluator/stub"
	"github.com/rhaqim/loom/schema"
	_ "modernc.org/sqlite"
)

// gateEngine builds a throwaway in-memory engine carrying ev. The semantic gate
// only needs the engine to reach its Evaluator, so nothing is seeded.
func gateEngine(t *testing.T, ev loom.Evaluator) *loom.Engine {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:semantic_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := loom.New(loom.Config{DB: db, Dialect: loom.DialectSQLite, Evaluator: ev})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSemanticGateRetriesOnCliche(t *testing.T) {
	ev := stub.New().WithNoul(qCliched, 0.92)
	hook := SemanticQualityGate(gateEngine(t, ev), 1.0)

	res := loom.NewTextResult("Some prose with no listed phrase in it at all.", "stop", 1, 1)
	_, err := hook(context.Background(), authorReq(""), res)
	if !loom.IsRetry(err) {
		t.Fatalf("a cliché verdict should trigger a retry, got %v", err)
	}
	ann, _ := loom.RetryAnnotationFrom(err)
	if !strings.Contains(ann.Reason, "clich") {
		t.Errorf("retry reason = %q, want the cliché guidance", ann.Reason)
	}
	// The point of the semantic tier: this draft contains none of the 21 banned
	// phrases, so SlopBanHook would have waved it through.
	if _, err := SlopBanHook()(context.Background(), authorReq(""), res); err != nil {
		t.Fatalf("the string hook should pass this draft; the two tiers are not redundant: %v", err)
	}
}

func TestSemanticGateRetriesOnRepetition(t *testing.T) {
	ev := stub.New().WithNoul(qRepeats, 0.88)
	hook := SemanticQualityGate(gateEngine(t, ev), 1.0)

	prev := "She crossed the bridge at dawn and the city opened below her."
	// Same scene, no shared trigrams — exactly what the Jaccard detector misses.
	res := loom.NewTextResult("At first light the span carried her over, and beneath lay the whole town.", "stop", 1, 1)

	if _, err := hook(context.Background(), authorReq(prev), res); !loom.IsRetry(err) {
		t.Fatalf("a repetition verdict should trigger a retry, got %v", err)
	}
	if _, err := StalenessHook(0)(context.Background(), authorReq(prev), res); err != nil {
		t.Fatalf("the trigram hook should pass this rewrite; that gap is why the semantic tier exists: %v", err)
	}
}

func TestSemanticGateSkipsTheRepetitionQuestionOnTheFirstScene(t *testing.T) {
	// With no previous scene there is nothing to compare against, so the
	// question must not be asked at all rather than asked against "".
	ev := stub.New()
	hook := SemanticQualityGate(gateEngine(t, ev), 1.0)

	res := loom.NewTextResult("The opening scene.", "stop", 1, 1)
	if _, err := hook(context.Background(), authorReq(""), res); err != nil {
		t.Fatal(err)
	}
	calls := ev.Calls()
	if len(calls) != 1 {
		t.Fatalf("made %d evaluations, want 1", len(calls))
	}
	if _, asked := calls[0].Questions[qRepeats]; asked {
		t.Error("the repetition question was asked with no previous scene")
	}
	if _, asked := calls[0].Questions[qCliched]; !asked {
		t.Error("the cliché question should still be asked on the first scene")
	}
}

func TestSemanticGateGradesQuality(t *testing.T) {
	ctx := context.Background()
	res := loom.NewTextResult("draft", "stop", 1, 1)

	// Below the bar: sent back.
	low := stub.New().WithScore(qQuality, 0.4)
	if _, err := SemanticQualityGate(gateEngine(t, low), 1.0)(ctx, authorReq(""), res); !loom.IsRetry(err) {
		t.Fatalf("a 0.4 draft should be sent back under a 1.0 bar, got %v", err)
	}

	// Merely flat, but above the bar: ships. This is the graded behaviour the
	// binary string hooks cannot express.
	flat := stub.New().WithScore(qQuality, 1.2)
	if _, err := SemanticQualityGate(gateEngine(t, flat), 1.0)(ctx, authorReq(""), res); err != nil {
		t.Fatalf("a 1.2 draft should ship under a 1.0 bar, got %v", err)
	}

	// A zero bar turns the graded tier off entirely.
	off := stub.New().WithScore(qQuality, 0)
	if _, err := SemanticQualityGate(gateEngine(t, off), 0)(ctx, authorReq(""), res); err != nil {
		t.Fatalf("MinProseQuality 0 should disable the graded tier, got %v", err)
	}
}

func TestSemanticGateLeavesOtherAgentsAlone(t *testing.T) {
	ev := stub.New().WithNoul(qCliched, 0.99)
	hook := SemanticQualityGate(gateEngine(t, ev), 1.0)

	req := authorReq("")
	req.AgentSlug = AgentLogician
	if _, err := hook(context.Background(), req, loom.NewTextResult(`{"options":[]}`, "stop", 1, 1)); err != nil {
		t.Fatalf("the Logician should pass through untouched: %v", err)
	}
	if n := len(ev.Calls()); n != 0 {
		t.Errorf("evaluated the Logician %d times, want 0", n)
	}
}

func TestSemanticGateIsInertWithoutAnEvaluator(t *testing.T) {
	// The example must keep running for anyone with no TYPESAFE_API_KEY.
	hook := SemanticQualityGate(gateEngine(t, nil), 1.0)
	if _, err := hook(context.Background(), authorReq(""),
		loom.NewTextResult("anything", "stop", 1, 1)); err != nil {
		t.Fatalf("the gate should be inert with no evaluator configured: %v", err)
	}
}
