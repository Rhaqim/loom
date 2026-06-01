package loom

import "context"

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

// hookBus is the default in-process HookBus.
type hookBus struct {
	preNames  []string
	preHooks  []PreHook
	postNames []string
	postHooks []PostHook
}

func newHookBus() *hookBus { return &hookBus{} }

func (b *hookBus) RegisterPre(name string, h PreHook) {
	b.preNames = append(b.preNames, name)
	b.preHooks = append(b.preHooks, h)
}

func (b *hookBus) RegisterPost(name string, h PostHook) {
	b.postNames = append(b.postNames, name)
	b.postHooks = append(b.postHooks, h)
}

func (b *hookBus) RunPre(ctx context.Context, req *StepRequest) error {
	for _, h := range b.preHooks {
		if err := h(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

func (b *hookBus) RunPost(ctx context.Context, req *StepRequest, res Result) (Result, error) {
	for _, h := range b.postHooks {
		var err error
		res, err = h(ctx, req, res)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (b *hookBus) PreNames() []string  { return append([]string(nil), b.preNames...) }
func (b *hookBus) PostNames() []string { return append([]string(nil), b.postNames...) }
