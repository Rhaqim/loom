// Package typesafe implements loom's EXPERIMENTAL evaluator.Evaluator against
// TypeSafe's System One API — the Jev model family.
//
// One POST carries the state and every question; the answers come back keyed by
// the ids that were asked, as calibrated probability distributions. Questions
// are evaluated independently and in parallel on the provider side, so asking
// ten questions costs barely more than asking one and no answer degrades the
// others.
//
// Usage:
//
//	ev := typesafe.New(os.Getenv("TYPESAFE_API_KEY"))
//	e, _ := loom.New(loom.Config{ /* ... */ Evaluator: ev })
//
// The API key is required. See https://docs.typesafe.ai for the question and
// answer reference this package mirrors.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rhaqim/loom/evaluator"
)

const (
	// defaultBaseURL is TypeSafe's production API root.
	defaultBaseURL = "https://api.typesafe.ai/v1"
	// evaluatePath is the System One evaluation endpoint.
	evaluatePath = "/systemone"
	// DefaultModel is the alias for TypeSafe's current flagship model.
	DefaultModel = "jev-latest"

	// maxResponseBytes caps the answer body we will read. Answers are small and
	// bounded by the question count; the cap keeps a misbehaving endpoint from
	// exhausting memory.
	maxResponseBytes = 4 << 20
	// maxErrorBodyBytes caps how much of an error body is quoted back.
	maxErrorBodyBytes = 8 << 10
)

// Evaluator is a TypeSafe System One client. It implements
// evaluator.Evaluator and is safe for concurrent use.
type Evaluator struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client

	maxRetries int
	backoff    time.Duration
}

// New creates a TypeSafe evaluator using the flagship model alias and a 30s
// request timeout. An evaluation is a classification, not a generation, so it
// returns in well under a second in normal operation; the generous timeout is
// for the tail, not the typical case.
func New(apiKey string) *Evaluator {
	return &Evaluator{
		apiKey:     apiKey,
		model:      DefaultModel,
		baseURL:    defaultBaseURL,
		client:     &http.Client{Timeout: 30 * time.Second},
		maxRetries: 2,
		backoff:    250 * time.Millisecond,
	}
}

// WithModel pins a specific model instead of the moving "jev-latest" alias.
func (e *Evaluator) WithModel(model string) *Evaluator {
	if model != "" {
		e.model = model
	}
	return e
}

// WithBaseURL overrides the API root — for a test server, a proxy, or a
// self-hosted deployment.
func (e *Evaluator) WithBaseURL(url string) *Evaluator {
	if url != "" {
		e.baseURL = strings.TrimRight(url, "/")
	}
	return e
}

// WithHTTPClient supplies the http.Client used for requests, for callers who
// need their own transport, timeout, or instrumentation.
func (e *Evaluator) WithHTTPClient(c *http.Client) *Evaluator {
	if c != nil {
		e.client = c
	}
	return e
}

// WithRetries configures how many times a retryable failure (429 rate limit,
// 529 overloaded, or a 5xx) is retried, and the base delay for the exponential
// backoff between attempts. A negative count is ignored.
func (e *Evaluator) WithRetries(n int, backoff time.Duration) *Evaluator {
	if n >= 0 {
		e.maxRetries = n
	}
	if backoff > 0 {
		e.backoff = backoff
	}
	return e
}

// -----------------------------------------------------------------------
// Wire types
// -----------------------------------------------------------------------

type wireRequest struct {
	State     any                  `json:"state"`
	Model     string               `json:"model"`
	Questions map[string]wireQuest `json:"questions"`
}

type wireQuest struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type wireAnswer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Legend        map[string]string  `json:"legend"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// -----------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------

// ErrUnauthorized is returned for a 401: the API key is missing or invalid.
var ErrUnauthorized = errors.New("typesafe: unauthorized")

// ErrRateLimited is returned for a 429 that survived the configured retries.
var ErrRateLimited = errors.New("typesafe: rate limited")

// ErrOverloaded is returned for a 529 that survived the configured retries.
var ErrOverloaded = errors.New("typesafe: service overloaded")

// ErrInvalidRequest is returned for a 422: the request failed validation at the
// provider. It means a question was malformed in a way local validation did not
// catch.
var ErrInvalidRequest = errors.New("typesafe: invalid request")

// APIError carries a non-2xx response that has no more specific sentinel.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("typesafe: http %d: %s", e.StatusCode, e.Body)
}

// -----------------------------------------------------------------------
// Evaluate
// -----------------------------------------------------------------------

// Evaluate sends state and questions to the System One endpoint and returns one
// answer per question.
//
// Questions are validated locally first, so a malformed question fails without
// a round trip. The response is checked for completeness: if the provider
// returns fewer answers than questions asked, or an answer whose type does not
// match its question, that is an error rather than a partial result a caller
// might silently gate on.
func (e *Evaluator) Evaluate(ctx context.Context, state any, questions map[string]evaluator.Question) (evaluator.Evaluation, error) {
	if e.apiKey == "" {
		return evaluator.Evaluation{}, fmt.Errorf("%w: no API key configured", ErrUnauthorized)
	}
	if err := evaluator.ValidateQuestions(questions); err != nil {
		return evaluator.Evaluation{}, fmt.Errorf("typesafe: %w", err)
	}

	wq := make(map[string]wireQuest, len(questions))
	for id, q := range questions {
		wq[id] = wireQuest{
			Type:         string(q.Type),
			Instructions: q.Instructions,
			Criteria:     q.Criteria,
		}
	}
	body, err := json.Marshal(wireRequest{State: state, Model: e.model, Questions: wq})
	if err != nil {
		return evaluator.Evaluation{}, fmt.Errorf("typesafe: encode request: %w", err)
	}

	raw, err := e.post(ctx, body)
	if err != nil {
		return evaluator.Evaluation{}, err
	}
	var resp wireResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return evaluator.Evaluation{}, fmt.Errorf("typesafe: decode response: %w", err)
	}

	answers := make(evaluator.Answers, len(questions))
	for id, q := range questions {
		wa, ok := resp.Answers[id]
		if !ok {
			return evaluator.Evaluation{}, fmt.Errorf("typesafe: no answer returned for question %q", id)
		}
		if wa.Type != string(q.Type) {
			return evaluator.Evaluation{}, fmt.Errorf(
				"typesafe: question %q asked for %s but answer is %s", id, q.Type, wa.Type)
		}
		answers[id] = evaluator.Answer{
			Type:          evaluator.Type(wa.Type),
			Noul:          wa.Noul,
			Choice:        wa.Choice,
			Score:         wa.Score,
			Legend:        wa.Legend,
			Probabilities: wa.Probabilities,
			Confidence:    wa.Confidence,
		}
	}

	return evaluator.Evaluation{
		Model:   resp.Model,
		Answers: answers,
		Usage: evaluator.Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}, nil
}

// post sends the request, retrying the transient statuses with exponential
// backoff. A non-retryable status returns immediately with a typed error.
func (e *Evaluator) post(ctx context.Context, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		raw, err := e.postOnce(ctx, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		if attempt >= e.maxRetries || !retryable(err) {
			return nil, err
		}
		// Back off exponentially, but never past the caller's deadline: a
		// cancelled context ends the retry loop with its own error rather than
		// sleeping through the cancellation.
		delay := e.backoff << attempt
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("typesafe: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(delay):
		}
	}
}

func (e *Evaluator) postOnce(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+evaluatePath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("typesafe: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, classify(resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

// classify maps a status code onto the package's sentinel errors so callers can
// branch with errors.Is instead of matching on status numbers.
func classify(status int, body string) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, body)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s", ErrInvalidRequest, body)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrRateLimited, body)
	case 529:
		return fmt.Errorf("%w: %s", ErrOverloaded, body)
	}
	return &APIError{StatusCode: status, Body: body}
}

// retryable reports whether an error is worth another attempt: the provider's
// two documented transient statuses, plus any 5xx, plus a transport error.
func retryable(err error) bool {
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrOverloaded) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	// A transport-level failure (connection reset, DNS blip) carries no status
	// and is worth retrying; a local encode/build failure is not, and those are
	// returned before post is ever reached.
	return strings.Contains(err.Error(), "typesafe: http:")
}
