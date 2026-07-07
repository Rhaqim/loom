package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// syncErrGen fails synchronously, like a transport/adapter error.
type syncErrGen struct{}

func (syncErrGen) Modality() Modality { return ModalityText }
func (syncErrGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return nil, errors.New("dial tcp: connection refused")
}

// nilGen is a misbehaving adapter that returns (nil, nil) on success.
type nilGen struct{}

func (nilGen) Modality() Modality { return ModalityText }
func (nilGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return nil, nil
}

func mkSession(t *testing.T, e *Engine) *Session {
	t.Helper()
	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestErrInvalidConfig(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(empty) err = %v, want Is(ErrInvalidConfig)", err)
	}
	if _, err := New(Config{Dialect: DialectSQLite}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(no DB) err = %v, want Is(ErrInvalidConfig)", err)
	}
}

func TestConfigCacheTTL(t *testing.T) {
	db, err := sql.Open("sqlite", "file:ttltest?mode=memory")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Explicit TTL is honored.
	e, err := New(Config{DB: db, Dialect: DialectSQLite, CacheTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if e.cacheTTL != 5*time.Minute {
		t.Fatalf("cacheTTL = %v, want %v", e.cacheTTL, 5*time.Minute)
	}
	// Zero falls back to the default.
	e2, err := New(Config{DB: db, Dialect: DialectSQLite})
	if err != nil {
		t.Fatal(err)
	}
	if e2.cacheTTL != defaultCacheTTL {
		t.Fatalf("default cacheTTL = %v, want %v", e2.cacheTTL, defaultCacheTTL)
	}
}

func TestInvalidSchemaPrefixRejected(t *testing.T) {
	// A prefix that is not a safe identifier must be rejected, not interpolated
	// into SQL.
	db, err := sql.Open("sqlite", "file:prefixtest?mode=memory")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := New(Config{DB: db, Dialect: DialectSQLite, SchemaPrefix: "loom\"; DROP TABLE x;--"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(bad prefix) err = %v, want Is(ErrInvalidConfig)", err)
	}
}

func TestGeneratorReturningNilResultDegradesToError(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "nilres", map[string]Generator{"g": nilGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := mkSession(t, e)
	// Must return a GenerationError, not panic on a nil-result dereference.
	_, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
	var ge *GenerationError
	if !errors.As(err, &ge) || ge.Kind != GenerationEmpty {
		t.Fatalf("RunStep err = %v, want GenerationError{Empty}", err)
	}
}

func TestNotFoundError_KindAndKey(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "nferr", map[string]Generator{"g": okGen{}}, PollerConfig{})

	_, err := e.Agents().Get(ctx, "ghost", 3)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("agent get err = %v, want Is(ErrNotFound)", err)
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("agent get err = %v, want As(*NotFoundError)", err)
	}
	if nf.Kind != "agent" || nf.Key != "ghost@v3" {
		t.Fatalf("NotFoundError = %+v, want {agent ghost@v3}", nf)
	}

	_, err = e.Prompts().Get(ctx, "missing", 1)
	if !errors.As(err, &nf) || nf.Kind != "prompt" {
		t.Fatalf("prompt get err = %v, want NotFoundError kind=prompt", err)
	}
}

func TestErrGeneratorNotRegistered(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "genreg", map[string]Generator{"g": okGen{}}, PollerConfig{})
	// Agent references a generator slug that was never registered.
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "nope"}); err != nil {
		t.Fatal(err)
	}
	sess := mkSession(t, e)
	_, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
	if !errors.Is(err, ErrGeneratorNotRegistered) {
		t.Fatalf("RunStep err = %v, want Is(ErrGeneratorNotRegistered)", err)
	}
}

func TestGenerationError_Transport(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "gentransport", map[string]Generator{"g": syncErrGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := mkSession(t, e)
	_, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a"})
	var ge *GenerationError
	if !errors.As(err, &ge) {
		t.Fatalf("RunStep err = %v, want As(*GenerationError)", err)
	}
	if ge.Kind != GenerationTransport || ge.Provider != "g" {
		t.Fatalf("GenerationError = %+v, want kind=transport provider=g", ge)
	}
	if ge.Unwrap() == nil {
		t.Fatal("GenerationError.Unwrap() = nil, want the underlying transport error")
	}
}

func TestSchemaValidation_ExposesStructuredViolations(t *testing.T) {
	hook := SchemaValidationPostHook()
	req := &StepRequest{ResponseFormat: &ResponseFormat{
		Schema: map[string]any{
			"type":     "object",
			"required": []any{"name"},
		},
	}}
	// Missing the required "name" property → one violation.
	_, err := hook(context.Background(), req, &StructuredResult{Data: map[string]any{}})
	ann, ok := RetryAnnotationFrom(err)
	if !ok {
		t.Fatalf("hook err = %v, want a RetryError", err)
	}
	if len(ann.Violations) == 0 {
		t.Fatalf("RetryAnnotation.Violations empty; want structured violations, reason=%q", ann.Reason)
	}
}

func TestGenerationError_Rejected(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "genrej", map[string]Generator{"g": failingStreamGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{Slug: "a", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	sess := mkSession(t, e)
	// OnChunk set → streaming path, which surfaces the failed result.
	_, err := e.RunStep(ctx, sess, StepRequest{AgentSlug: "a", OnChunk: func(Chunk) {}})
	var ge *GenerationError
	if !errors.As(err, &ge) {
		t.Fatalf("RunStep err = %v, want As(*GenerationError)", err)
	}
	if ge.Kind != GenerationRejected {
		t.Fatalf("GenerationError kind = %q, want %q", ge.Kind, GenerationRejected)
	}
}
