package handler

import (
	"testing"
	"time"
)

func TestRateLimiterFixedWindow(t *testing.T) {
	now := time.Unix(0, 0)
	rl := newRateLimiter(3, time.Minute)
	rl.nowFn = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("4th request in window should be blocked")
	}

	// A different client is unaffected.
	if !rl.allow("5.6.7.8") {
		t.Fatal("distinct client should be allowed")
	}

	// After the window elapses, the original client is allowed again.
	now = now.Add(time.Minute + time.Second)
	if !rl.allow("1.2.3.4") {
		t.Fatal("request after window reset should be allowed")
	}
}
