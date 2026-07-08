package engine

import (
	"context"
	"time"
)

type asyncPollerService struct {
	e   *Engine
	cfg PollerConfig
}

func newAsyncPollerService(e *Engine, cfg PollerConfig) *asyncPollerService {
	return &asyncPollerService{e: e, cfg: cfg}
}

func (p *asyncPollerService) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	sem := make(chan struct{}, p.cfg.Workers)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Acquire a worker slot, but stay responsive to cancellation: if all
			// workers are busy on slow polls, don't block here past shutdown.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				defer func() { <-sem }()
				p.pollPending(ctx)
			}()
		}
	}
}

func (p *asyncPollerService) pollPending(ctx context.Context) {
	tasks, err := sqlListPendingTasks(ctx, p.e.db, p.e.prefix)
	if err != nil {
		p.e.log.Error("poll pending tasks", "err", err)
		return
	}
	for _, t := range tasks {
		gen, ok := p.e.generator(t.Provider)
		if !ok {
			continue
		}
		poller, ok := gen.(AsyncGenerator)
		if !ok {
			continue
		}
		result, err := poller.Poll(ctx, TaskHandle{
			ID:       t.ID,
			Provider: t.Provider,
			Handle:   t.Handle,
		})
		switch {
		case err != nil:
			p.e.log.Error("poll task", "task_id", t.ID, "err", err)
			p.retryOrFail(ctx, t, err.Error())
		case result == nil:
			// Defensive: a generator returning (nil, nil) is a non-terminal miss,
			// not a worker panic.
			p.retryOrFail(ctx, t, "poll returned no result")
		case result.Status() == ResultStatusReady:
			if err := sqlMarkTaskReady(ctx, p.e.db, p.e.prefix, t.ID, result); err != nil {
				p.e.log.Error("mark task ready", "task_id", t.ID, "err", err)
			} else {
				p.notify(ctx, t, ResultStatusReady, result, "")
			}
		case result.Status() == ResultStatusFailed:
			msg := resultErrorMessage(result)
			if err := sqlMarkTaskFailed(ctx, p.e.db, p.e.prefix, t.ID, msg); err != nil {
				p.e.log.Error("mark task failed", "task_id", t.ID, "err", err)
			}
			p.notify(ctx, t, ResultStatusFailed, nil, msg)
		default:
			// Still pending at the provider: count the poll so a job that never
			// resolves is eventually capped rather than polled forever.
			p.retryOrFail(ctx, t, "still pending after maximum poll attempts")
		}
	}
}

// defaultMaxPollAttempts caps how many times a task is polled before it is
// terminally failed when PollerConfig.MaxAttempts is unset.
const defaultMaxPollAttempts = 60

func (p *asyncPollerService) maxAttempts() int {
	if p.cfg.MaxAttempts > 0 {
		return p.cfg.MaxAttempts
	}
	return defaultMaxPollAttempts
}

// retryOrFail increments a task's attempt counter, or terminally fails it once it
// reaches the attempt cap, so neither erroring nor perpetually-pending jobs are
// polled forever.
func (p *asyncPollerService) retryOrFail(ctx context.Context, t pendingTask, reason string) {
	if t.Attempts+1 >= p.maxAttempts() {
		if err := sqlMarkTaskFailed(ctx, p.e.db, p.e.prefix, t.ID, reason); err != nil {
			p.e.log.Error("mark task failed", "task_id", t.ID, "err", err)
		}
		p.notify(ctx, t, ResultStatusFailed, nil, reason)
	} else {
		if err := sqlIncrementTaskAttempts(ctx, p.e.db, p.e.prefix, t.ID, reason); err != nil {
			p.e.log.Error("increment task attempts", "task_id", t.ID, "err", err)
		}
	}
}

// notify delivers a task's terminal resolution to the registered callback, if
// any. It runs inline in the poller goroutine; the callback is documented as
// fast and non-blocking.
func (p *asyncPollerService) notify(ctx context.Context, t pendingTask, status ResultStatus, result Result, errMsg string) {
	fn := p.e.taskResolvedHook()
	if fn == nil {
		return
	}
	fn(ctx, TaskResolution{
		TaskID:    t.ID,
		SessionID: t.SessionID,
		AgentID:   t.AgentID,
		Provider:  t.Provider,
		Status:    status,
		Result:    result,
		Err:       errMsg,
	})
}

// resultErrorMessage extracts a failed result's error string, falling back to a
// generic message.
func resultErrorMessage(r Result) string {
	if r != nil {
		if e, ok := r.Metadata()["error"].(string); ok && e != "" {
			return e
		}
	}
	return "provider reported failure"
}
