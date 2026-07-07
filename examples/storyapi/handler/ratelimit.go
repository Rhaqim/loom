package handler

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a minimal per-key fixed-window limiter used to throttle
// unauthenticated auth endpoints (login/register), blunting brute-force,
// credential-stuffing, and automated account-enumeration attacks. It is
// in-process and best-effort — a production deployment behind multiple
// instances should use a shared store (e.g. Redis).
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string]*window
	limit  int
	window time.Duration
	lastGC time.Time
	nowFn  func() time.Time // injectable for tests
}

type window struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, per time.Duration) *rateLimiter {
	return &rateLimiter{
		hits:   make(map[string]*window),
		limit:  limit,
		window: per,
		nowFn:  time.Now,
	}
}

// allow reports whether a request from key may proceed, consuming one unit.
func (rl *rateLimiter) allow(key string) bool {
	now := rl.nowFn()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Opportunistic GC of expired windows so the map does not grow unbounded
	// under a flood of distinct source IPs.
	if now.Sub(rl.lastGC) > rl.window {
		for k, w := range rl.hits {
			if now.After(w.reset) {
				delete(rl.hits, k)
			}
		}
		rl.lastGC = now
	}

	w, ok := rl.hits[key]
	if !ok || now.After(w.reset) {
		rl.hits[key] = &window{count: 1, reset: now.Add(rl.window)}
		return true
	}
	if w.count >= rl.limit {
		return false
	}
	w.count++
	return true
}

// limit wraps a handler, rejecting requests from a client IP that exceeds the
// configured rate with 429.
func (rl *rateLimiter) middleware(h *Handler, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			h.fail(w, http.StatusTooManyRequests, "too many requests, please try again later")
			return
		}
		next(w, r)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
