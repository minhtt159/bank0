package api

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rateLimiter is a tiny in-memory sliding-window limiter keyed by an arbitrary
// string (client IP, subject, ...). It is a PER-INSTANCE backstop: the edge
// (Cloudflare) is the primary control, and a multi-replica deployment would need a
// shared store (Redis/DB) for a global limit. It is enough to blunt credential
// brute-force / token-guessing against a single instance. See docs/10.
type rateLimiter struct {
	mu        sync.Mutex
	hits      map[string][]time.Time
	limit     int
	window    time.Duration
	lastSweep time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: map[string][]time.Time{}, limit: limit, window: window}
}

// allow records an event for key at now and reports whether key is within the
// limit over the trailing window.
func (rl *rateLimiter) allow(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := now.Add(-rl.window)
	rl.sweep(now, cutoff)

	kept := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, now)
	return true
}

// sweep evicts keys with no events inside the window, bounding map growth under a
// flood of distinct keys. Runs at most once per window. Caller holds the lock.
func (rl *rateLimiter) sweep(now, cutoff time.Time) {
	if now.Sub(rl.lastSweep) < rl.window {
		return
	}
	rl.lastSweep = now
	for k, ev := range rl.hits {
		fresh := false
		for _, t := range ev {
			if t.After(cutoff) {
				fresh = true
				break
			}
		}
		if !fresh {
			delete(rl.hits, k)
		}
	}
}

// rateLimit wraps next with rl, keyed by keyFn. A nil rl (disabled, e.g. tests or
// rate_limit_per_min=0) is a pass-through. On exceed: 429 + Retry-After.
func (s *Server) rateLimit(rl *rateLimiter, keyFn func(*http.Request) string, next http.Handler) http.Handler {
	if rl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(keyFn(r), time.Now()) {
			w.Header().Set("Retry-After", strconv.Itoa(int(rl.window.Seconds())))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}
