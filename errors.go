package loom

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
}

// RetryError signals a post-hook wants the step re-executed with updated hints.
type RetryError struct {
	Annotation RetryAnnotation
}

func (e *RetryError) Error() string {
	return fmt.Sprintf("loom: retry: %s", e.Annotation.Reason)
}

// ErrRetryWith constructs a RetryError with the given annotation.
func ErrRetryWith(ann RetryAnnotation) error {
	return &RetryError{Annotation: ann}
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

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("loom: not found")

// ErrDuplicateSlug is returned when an agent or prompt slug+version already exists.
var ErrDuplicateSlug = errors.New("loom: duplicate slug+version")
