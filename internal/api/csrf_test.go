package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// csrfGuard rejects cross-origin state-changing requests on the cookie surface,
// while allowing safe methods and non-browser callers (no Origin/Referer).
func TestCSRFGuard(t *testing.T) {
	s := &Server{}
	h := s.csrfGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	do := func(method, host, origin, referer string) int {
		req := httptest.NewRequest(method, "http://"+host+"/console/disputes/x/resolve", nil)
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	cases := []struct {
		name             string
		method, origin, ref string
		want             int
	}{
		{"safe GET cross-origin", http.MethodGet, "https://evil.example", "", 200},
		{"POST no origin (non-browser)", http.MethodPost, "", "", 200},
		{"POST same-origin", http.MethodPost, "https://portal.bank0", "", 200},
		{"POST cross-origin via Origin", http.MethodPost, "https://evil.example", "", 403},
		{"POST cross-origin via Referer", http.MethodPost, "", "https://evil.example/x", 403},
	}
	for _, c := range cases {
		if got := do(c.method, "portal.bank0", c.origin, c.ref); got != c.want {
			t.Errorf("%s = %d, want %d", c.name, got, c.want)
		}
	}
}
