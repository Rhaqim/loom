package engine

import (
	"errors"
	"fmt"
	"time"
)

// skipError is returned by a pre-hook to cancel a step without treating it as failure.
type skipError struct{ reason string }

func (e skipError) Error() string { return "loom: skip: " + e.reason }

// ErrSkip cancels the current step when returned from a pre-hook.
var ErrSkip = skipError{reason: "step cancelled by pre-hook"}

// IsSkip reports whether err signals a skip.
func IsSkip(err error) bool {
	var s skipError
	return errors.As(err, &s)
}

// RetryAnnotation carries metadata about why a retry was requested.
type RetryAnnotation struct {
	Reason    string   // human-readable reason
	Forbidden []string // phrases/patterns the next attempt must avoid
	// Violations lists structured schema-validation failures when the retry was
	// triggered by SchemaValidationPostHook (each entry is a "path: reason").
	// Empty for retries requested for other reasons. Read it via
	// RetryAnnotationFrom to react to validation failures programmatically.
	Violations []string
}

// RetryError signals a post-hook wants the step re-executed with updated hints.
type RetryError struct {
	Annotation RetryAnnotation
	// Score rates the rejected draft; lower is better. Used only under
	// RetryKeepBest to decide which attempt to keep. Zero when unused.
	Score int
	// Draft is the (hook-transformed) result eligible to be kept under
	// RetryKeepBest when every attempt is rejected. Nil means not keepable.
	Draft Result
}

func (e *RetryError) Error() string {
	return fmt.Sprintf("loom: retry: %s", e.Annotation.Reason)
}

// ErrRetryWith constructs a RetryError with the given annotation.
func ErrRetryWith(ann RetryAnnotation) error {
	return &RetryError{Annotation: ann}
}

// ErrRetryScored is ErrRetryWith plus a score for the rejected draft and the
// draft itself, for RetryKeepBest steps: the engine keeps the lowest-scoring
// draft across attempts rather than failing on exhaustion. A non-positive score
// with a non-nil draft signals acceptance (keep it and stop retrying).
func ErrRetryScored(ann RetryAnnotation, score int, draft Result) error {
	return &RetryError{Annotation: ann, Score: score, Draft: draft}
}

// IsRetry reports whether err is a RetryError.
func IsRetry(err error) bool {
	var r *RetryError
	return errors.As(err, &r)
}

// RetryAnnotationFrom extracts the RetryAnnotation if err is a RetryError.
func RetryAnnotationFrom(err error) (RetryAnnotation, bool) {
	var r *RetryError
	if errors.As(err, &r) {
		return r.Annotation, true
	}
	return RetryAnnotation{}, false
}

// BudgetExceededError is returned when a step would exceed a configured budget.
type BudgetExceededError struct {
	BudgetName  string
	Window      string
	UsedUSD     float64
	LimitUSD    float64
	UsedTokens  int
	LimitTokens int
	ResetsAt    time.Time
	Suggestion  string
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("loom: budget exceeded: %s (%s window): used $%.4f of $%.4f",
		e.BudgetName, e.Window, e.UsedUSD, e.LimitUSD)
}

// ErrNotFound is returned when a requested entity does not exist. Errors from
// the registries (Agents/Prompts/Sessions/Budgets/ResponseFormats) unwrap to it,
// so errors.Is(err, ErrNotFound) is the portable "does not exist" check; use
// errors.As with *NotFoundError to learn which entity and key were missing.
var ErrNotFound = errors.New("loom: not found")

// NotFoundError identifies which entity was missing and by what key. It unwraps
// to ErrNotFound, so both errors.Is(err, ErrNotFound) and
// errors.As(err, &loom.NotFoundError{}) work.
type NotFoundError struct {
	Kind string // "agent", "prompt", "session", "budget", "response_format"
	Key  string // the lookup key: slug, "slug@vN", or a UUID
}

func (e *NotFoundError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("loom: %s not found", e.Kind)
	}
	return fmt.Sprintf("loom: %s %q not found", e.Kind, e.Key)
}

func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// wrapNotFound converts a bare ErrNotFound into a keyed *NotFoundError, leaving
// any other error (including nil) untouched.
func wrapNotFound(err error, kind, key string) error {
	if errors.Is(err, ErrNotFound) {
		return &NotFoundError{Kind: kind, Key: key}
	}
	return err
}

// ErrDuplicateSlug is returned when an agent or prompt slug+version already exists.
var ErrDuplicateSlug = errors.New("loom: duplicate slug+version")

// ErrSessionConflict is returned by Session update when the row's version no
// longer matches the in-memory copy — another writer updated it first, so this
// write would clobber theirs (optimistic-concurrency conflict).
var ErrSessionConflict = errors.New("loom: session update conflict")

// ErrMissingVariables is returned by RunStep when the user template's prompt
// declares required input variables (Prompt.Variables) and one or more were not
// supplied in StepRequest.Inputs. The wrapped message lists the missing names.
// A prompt that declares no variables never triggers this.
var ErrMissingVariables = errors.New("loom: missing required template variables")

// ErrSessionPinned is returned by Purge when the target session is pinned.
// Pinning marks data as protected, so an irreversible delete requires the
// explicit ForcePurge instead.
var ErrSessionPinned = errors.New("loom: session is pinned")

// ErrSessionNotPersisted is returned by RunStep (and wrapped into Turn.Errors
// by RunTurn) when a step ran and committed — its result and checkpoint are
// durable — but the subsequent loom_sessions row update failed. The returned
// *Step is valid and usable; what is stale is loom_sessions.state, and any
// in-memory *Session the caller holds.
//
// It always wraps a more specific cause: ErrSessionConflict when another writer
// advanced the row, ErrNotFound when the row was discarded mid-step, or a raw
// driver error. Recover the authoritative post-step state with
// SessionRegistry.StateAt(ctx, id, step.Index) and retry the write, or re-Get
// the session and replay.
//
// This is deliberately a returned error rather than a diagnostics flag: a
// caller using loom as a system of record must not proceed on state the
// database does not have.
var ErrSessionNotPersisted = errors.New("loom: session row not persisted after step")

// ErrInvalidConfig is returned by New when the Config is missing a required
// field or is otherwise unusable. Wrapped errors carry the specific reason.
var ErrInvalidConfig = errors.New("loom: invalid config")

// ErrGeneratorNotRegistered is returned when a step references a generator slug
// that was never registered with the engine. Wrapped errors name the slug.
var ErrGeneratorNotRegistered = errors.New("loom: generator not registered")

// GenerationErrorKind classifies why a generator failed to produce a result, so
// applications can react differently (e.g. retry transport failures but surface
// provider rejections to the user).
type GenerationErrorKind string

const (
	// GenerationTransport is a network, stream, or context failure reaching or
	// reading from the provider. The underlying error is available via Unwrap.
	GenerationTransport GenerationErrorKind = "transport"
	// GenerationRejected means the provider ran but returned a failed result
	// (content policy, safety refusal, provider-side error).
	GenerationRejected GenerationErrorKind = "rejected"
	// GenerationEmpty means the provider returned no usable result.
	GenerationEmpty GenerationErrorKind = "empty"
)

// GenerationError is returned when a generator fails to produce a usable result.
// Inspect it with errors.As(err, &loom.GenerationError{}) and switch on Kind.
type GenerationError struct {
	Kind     GenerationErrorKind
	Provider string // generator slug, when known
	Message  string // provider-supplied detail, when any
	Err      error  // underlying transport error, when any
}

func (e *GenerationError) Error() string {
	msg := "loom: generation failed"
	if e.Provider != "" {
		msg += fmt.Sprintf(" [%s]", e.Provider)
	}
	msg += fmt.Sprintf(" (%s)", e.Kind)
	switch {
	case e.Message != "":
		msg += ": " + e.Message
	case e.Err != nil:
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *GenerationError) Unwrap() error { return e.Err }
