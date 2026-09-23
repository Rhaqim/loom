package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rhaqim/loom/evaluator"
)

var _ evaluator.Evaluator = (*Evaluator)(nil)

// serve stands up a fake System One endpoint returning body with status, and
// captures the last request it received.
func serve(t *testing.T, status int, body string) (*Evaluator, *http.Request, *[]byte) {
	t.Helper()
	var gotReq http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = *r
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New("test-key").WithBaseURL(srv.URL), &gotReq, &gotBody
}

func TestEvaluateSendsTheDocumentedRequestShape(t *testing.T) {
	ev, gotReq, gotBody := serve(t, 200, `{
		"model": "jev-1.13.0",
		"answers": {"is_urgent": {"type": "noul", "noul": 0.95}},
		"usage": {"input_tokens": 296, "output_tokens": 20}
	}`)

	eval, err := ev.Evaluate(context.Background(), "payouts failing for 3 days",
		map[string]evaluator.Question{"is_urgent": evaluator.Noul("Does this convey urgency?")})
	if err != nil {
		t.Fatal(err)
	}

	if got := gotReq.URL.Path; got != "/systemone" {
		t.Errorf("path = %q, want /systemone", got)
	}
	if got := gotReq.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want the bearer key", got)
	}
	if got := gotReq.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var sent map[string]any
	if err := json.Unmarshal(*gotBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["state"] != "payouts failing for 3 days" {
		t.Errorf("state = %v, want the passed state verbatim", sent["state"])
	}
	if sent["model"] != DefaultModel {
		t.Errorf("model = %v, want %q", sent["model"], DefaultModel)
	}
	q := sent["questions"].(map[string]any)["is_urgent"].(map[string]any)
	if q["type"] != "noul" {
		t.Errorf("question type = %v, want noul", q["type"])
	}
	// criteria is omitempty: an unadorned Noul must not send a null field.
	if _, present := q["criteria"]; present {
		t.Errorf("criteria present on a bare Noul: %v", q)
	}

	if eval.Model != "jev-1.13.0" {
		t.Errorf("Model = %q, want the concrete model from the response", eval.Model)
	}
	if got := eval.Answers.Noul("is_urgent"); got != 0.95 {
		t.Errorf("noul = %v, want 0.95", got)
	}
	if eval.Usage.InputTokens != 296 || eval.Usage.OutputTokens != 20 {
		t.Errorf("Usage = %+v, want 296/20", eval.Usage)
	}
}

func TestEvaluateParsesChoiceAndScore(t *testing.T) {
	ev, _, _ := serve(t, 200, `{
		"model": "jev-1.13.0",
		"answers": {
			"department": {
				"type": "choice",
				"choice": "billing",
				"probabilities": {"billing": 0.88, "technical": 0.12, "sales": 0.0},
				"confidence": 0.81
			},
			"frustration": {
				"type": "score",
				"score": 1.05,
				"legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},
				"probabilities": {"0": 0.0, "1": 0.95, "2": 0.05},
				"confidence": 0.92
			}
		},
		"usage": {"input_tokens": 318, "output_tokens": 34}
	}`)

	eval, err := ev.Evaluate(context.Background(), "state", map[string]evaluator.Question{
		"department": evaluator.Choice("Which team?", map[string]any{
			"billing": "Payments", "technical": "Bugs", "sales": "Pricing"}),
		"frustration": evaluator.Score("How frustrated?", "Calm", "Frustrated", "Very angry"),
	})
	if err != nil {
		t.Fatal(err)
	}

	dept := eval.Answers.Get("department")
	if !dept.Is("billing") {
		t.Errorf("choice = %q, want billing", dept.Choice)
	}
	if dept.P("technical") != 0.12 {
		t.Errorf("P(technical) = %v, want 0.12", dept.P("technical"))
	}
	if !dept.Certain(0.8) {
		t.Error("Certain(0.8) = false at confidence 0.81, want true")
	}
	if got := dept.Ranked(); got[0].Key != "billing" {
		t.Errorf("Ranked()[0] = %q, want billing", got[0].Key)
	}

	frus := eval.Answers.Get("frustration")
	if frus.Score != 1.05 {
		t.Errorf("score = %v, want 1.05", frus.Score)
	}
	if frus.Level() != 1 || frus.Legend["1"] != "Frustrated" {
		t.Errorf("Level()/legend = %d/%q, want 1/Frustrated", frus.Level(), frus.Legend["1"])
	}
}

func TestEvaluateRejectsAMissingAnswer(t *testing.T) {
	// A partial result is the dangerous failure: a gate reading the missing
	// question would silently take its fail-open branch on every turn.
	ev, _, _ := serve(t, 200, `{"model":"m","answers":{"a":{"type":"noul","noul":0.9}},"usage":{}}`)
	_, err := ev.Evaluate(context.Background(), "s", map[string]evaluator.Question{
		"a": evaluator.Noul("q1"),
		"b": evaluator.Noul("q2"),
	})
	if err == nil || !strings.Contains(err.Error(), `question "b"`) {
		t.Fatalf("Evaluate() = %v, want an error naming the unanswered question", err)
	}
}

func TestEvaluateRejectsAMismatchedAnswerType(t *testing.T) {
	ev, _, _ := serve(t, 200, `{"model":"m","answers":{"a":{"type":"score","score":1}},"usage":{}}`)
	_, err := ev.Evaluate(context.Background(), "s",
		map[string]evaluator.Question{"a": evaluator.Noul("q")})
	if err == nil || !strings.Contains(err.Error(), "asked for noul") {
		t.Fatalf("Evaluate() = %v, want a type-mismatch error", err)
	}
}

func TestEvaluateValidatesBeforeTheRoundTrip(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	ev := New("k").WithBaseURL(srv.URL)
	_, err := ev.Evaluate(context.Background(), "s",
		map[string]evaluator.Question{"bad": evaluator.Score("rate", "only-one-level")})
	if err == nil {
		t.Fatal("Evaluate() with a malformed question = nil error, want a failure")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("made %d requests for a locally-invalid question, want 0", n)
	}
}

func TestEvaluateRequiresAnAPIKey(t *testing.T) {
	_, err := New("").Evaluate(context.Background(), "s",
		map[string]evaluator.Question{"a": evaluator.Noul("q")})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Evaluate() = %v, want ErrUnauthorized", err)
	}
}

func TestErrorStatusesMapToSentinels(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{401, ErrUnauthorized},
		{422, ErrInvalidRequest},
	}
	for _, tc := range tests {
		ev, _, _ := serve(t, tc.status, `{"error":"nope"}`)
		_, err := ev.Evaluate(context.Background(), "s",
			map[string]evaluator.Question{"a": evaluator.Noul("q")})
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d = %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestRetriesTransientStatusesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"m","answers":{"a":{"type":"noul","noul":0.8}},"usage":{}}`)
	}))
	defer srv.Close()

	ev := New("k").WithBaseURL(srv.URL).WithRetries(2, time.Millisecond)
	eval, err := ev.Evaluate(context.Background(), "s",
		map[string]evaluator.Question{"a": evaluator.Noul("q")})
	if err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("made %d attempts, want 3 (two 429s then a success)", n)
	}
	if eval.Answers.Noul("a") != 0.8 {
		t.Errorf("noul = %v, want 0.8", eval.Answers.Noul("a"))
	}
}

func TestGivesUpAfterTheRetryBudget(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"error":"overloaded"}`)
	}))
	defer srv.Close()

	ev := New("k").WithBaseURL(srv.URL).WithRetries(1, time.Millisecond)
	_, err := ev.Evaluate(context.Background(), "s",
		map[string]evaluator.Question{"a": evaluator.Noul("q")})
	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("Evaluate() = %v, want ErrOverloaded", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("made %d attempts, want 2 (the initial call plus one retry)", n)
	}
}

func TestDoesNotRetryAValidationFailure(t *testing.T) {
	// A 422 means the request itself is wrong; retrying it just burns the
	// caller's deadline for the same answer.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	ev := New("k").WithBaseURL(srv.URL).WithRetries(3, time.Millisecond)
	if _, err := ev.Evaluate(context.Background(), "s",
		map[string]evaluator.Question{"a": evaluator.Noul("q")}); err == nil {
		t.Fatal("want an error")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("made %d attempts on a 422, want 1", n)
	}
}

func TestRetryLoopHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	ev := New("k").WithBaseURL(srv.URL).WithRetries(10, time.Hour)
	start := time.Now()
	if _, err := ev.Evaluate(ctx, "s",
		map[string]evaluator.Question{"a": evaluator.Noul("q")}); err == nil {
		t.Fatal("want an error")
	}
	// A backoff that ignored the deadline would sleep for an hour.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v to give up, want the context deadline to cut the backoff short", elapsed)
	}
}

func TestWithModelAndBaseURLIgnoreEmptyValues(t *testing.T) {
	ev := New("k")
	base, model := ev.baseURL, ev.model
	ev.WithModel("").WithBaseURL("")
	if ev.baseURL != base || ev.model != model {
		t.Error("empty option values should leave the defaults in place")
	}
	ev.WithBaseURL("https://example.test/v1/")
	if ev.baseURL != "https://example.test/v1" {
		t.Errorf("baseURL = %q, want the trailing slash trimmed", ev.baseURL)
	}
}
