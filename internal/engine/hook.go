package engine

import (
	"context"
	"sync"
)

// PreHook runs before the agent. It may modify the request, cancel the step
// (return ErrSkip), or annotate the state. Hooks run in registration order.
type PreHook func(ctx context.Context, req *StepRequest) error

// PostHook runs after the agent, before the result is persisted. It may
// validate, annotate, transform, or request a retry (return ErrRetryWith(...)).
type PostHook func(ctx context.Context, req *StepRequest, res Result) (Result, error)

// HookBus manages pre- and post-hook registration.
type HookBus interface {
	// RegisterPre adds a named pre-hook. Hooks run in registration order.
	RegisterPre(name string, h PreHook)
	// RegisterPost adds a named post-hook. Hooks run in registration order.
	RegisterPost(name string, h PostHook)
	// RunPre executes all registered pre-hooks in order.
	// Returns the first non-nil error; ErrSkip propagates as-is.
	RunPre(ctx context.Context, req *StepRequest) error
	// RunPost executes all registered post-hooks in order.
	// A RetryError from any hook propagates immediately; the result at that
	// point is discarded and the step runner retries the agent call.
	RunPost(ctx context.Context, req *StepRequest, res Result) (Result, error)
	// Names returns registered hook names in order, separated by kind.
	PreNames() []string
	PostNames() []string
}

// hookBus is the default in-process HookBus. All access is guarded by mu so
// that registering a hook concurrently with a running step (RunStep/RunTurn fan
// out steps across goroutines that share the engine's bus) is race-free: Run*
// snapshots the slices under a read lock before invoking any hook, so a hook is
// never invoked while the slice header is being mutated.
type hookBus struct {
	mu        sync.RWMutex
	preNames  []string
	preHooks  []PreHook
	postNames []string
	postHooks []PostHook
}

func newHookBus() *hookBus { return &hookBus{} }

func (b *hookBus) RegisterPre(name string, h PreHook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.preNames = append(b.preNames, name)
	b.preHooks = append(b.preHooks, h)
}

func (b *hookBus) RegisterPost(name string, h PostHook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.postNames = append(b.postNames, name)
	b.postHooks = append(b.postHooks, h)
}

func (b *hookBus) RunPre(ctx context.Context, req *StepRequest) error {
	b.mu.RLock()
	hooks := b.preHooks[:len(b.preHooks):len(b.preHooks)]
	b.mu.RUnlock()
	for _, h := range hooks {
		if err := h(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

func (b *hookBus) RunPost(ctx context.Context, req *StepRequest, res Result) (Result, error) {
	b.mu.RLock()
	hooks := b.postHooks[:len(b.postHooks):len(b.postHooks)]
	b.mu.RUnlock()
	for _, h := range hooks {
		var err error
		res, err = h(ctx, req, res)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (b *hookBus) PreNames() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]string(nil), b.preNames...)
}

func (b *hookBus) PostNames() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]string(nil), b.postNames...)
}
