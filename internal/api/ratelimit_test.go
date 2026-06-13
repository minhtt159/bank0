package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterAllow(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	base := time.Unix(1_700_000_000, 0)

	for i := 0; i < 3; i++ {
		if !rl.allow("ip1", base.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("hit %d within limit should be allowed", i)
		}
	}
	if rl.allow("ip1", base.Add(4*time.Second)) {
		t.Error("4th hit within the window should be denied")
	}
	if !rl.allow("ip2", base.Add(4*time.Second)) {
		t.Error("a different key must be independent")
	}
	if !rl.allow("ip1", base.Add(time.Minute+time.Second)) {
		t.Error("once the window slides past, the key should be allowed again")
	}
}

func TestRateLimitMiddleware429(t *testing.T) {
	s := &Server{}
	rl := newRateLimiter(2, time.Minute)
	key := func(*http.Request) string { return "k" }
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	h := s.rateLimit(rl, key, ok)
	var last *httptest.ResponseRecorder
	codes := []int{}
	for i := 0; i < 3; i++ {
		last = httptest.NewRecorder()
		h.ServeHTTP(last, httptest.NewRequest(http.MethodPost, "/auth/login", nil))
		codes = append(codes, last.Code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Errorf("codes = %v, want [200 200 429]", codes)
	}
	if got := last.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}

	// a nil limiter (rate_limit_per_min=0 / tests) is a pass-through
	rec := httptest.NewRecorder()
	s.rateLimit(nil, key, ok).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != 200 {
		t.Errorf("nil limiter should pass through, got %d", rec.Code)
	}
}
