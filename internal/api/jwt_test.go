package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/config"
	"github.com/minhtt159/bank0/internal/db"
)

func jwtServer(secret string, ttl time.Duration) *Server {
	return &Server{
		jwtSecret: []byte(secret),
		jwtTTL:    ttl,
		cfg: config.Config{Auth: config.AuthConfig{
			JWTIssuer:   "bank0",
			JWTAudience: "bank0-client",
		}},
	}
}

func TestJWTRoundTrip(t *testing.T) {
	s := jwtServer("topsecret", time.Hour)
	uid := uuid.New()

	tok, exp, err := s.issueJWT(db.Principal{UserID: uid, Role: "customer", Username: "alice"}, []string{"pwd"}, "")
	if err != nil {
		t.Fatalf("issueJWT: %v", err)
	}
	if exp.Before(time.Now().Add(50 * time.Minute)) {
		t.Errorf("exp should be ~1h out; got %v", exp)
	}

	claims, err := s.parseJWT(tok)
	if err != nil {
		t.Fatalf("parseJWT: %v", err)
	}
	if claims.Subject != uid.String() {
		t.Errorf("sub = %q, want %q", claims.Subject, uid.String())
	}
	if claims.Role != "customer" || claims.Username != "alice" {
		t.Errorf("claims role/username = %q/%q", claims.Role, claims.Username)
	}
}

func TestJWTRejectsTamperedSecret(t *testing.T) {
	tok, _, _ := jwtServer("secretA", time.Hour).issueJWT(db.Principal{UserID: uuid.New(), Role: "customer", Username: "a"}, []string{"pwd"}, "")
	if _, err := jwtServer("secretB", time.Hour).parseJWT(tok); err == nil {
		t.Error("token signed with a different secret must be rejected")
	}
}

func TestJWTRejectsExpired(t *testing.T) {
	s := jwtServer("topsecret", -time.Hour) // already expired
	tok, _, _ := s.issueJWT(db.Principal{UserID: uuid.New(), Role: "customer", Username: "a"}, []string{"pwd"}, "")
	if _, err := s.parseJWT(tok); err == nil {
		t.Error("expired token must be rejected")
	}
}

func TestJWTRejectsWrongAudience(t *testing.T) {
	tok, _, _ := jwtServer("topsecret", time.Hour).issueJWT(db.Principal{UserID: uuid.New(), Role: "customer", Username: "a"}, []string{"pwd"}, "")
	other := jwtServer("topsecret", time.Hour)
	other.cfg.Auth.JWTAudience = "some-other-aud"
	if _, err := other.parseJWT(tok); err == nil {
		t.Error("token for a different audience must be rejected")
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc.def": "abc.def",
		"bearer abc.def": "abc.def", // case-insensitive scheme
		"Basic abc":      "",
		"":               "",
		"Bearer ":        "",
	}
	for hdr, want := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if hdr != "" {
			r.Header.Set("Authorization", hdr)
		}
		if got := bearerToken(r); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", hdr, got, want)
		}
	}
}

func TestClientSubject(t *testing.T) {
	uid := uuid.New()
	ctx := context.WithValue(context.Background(), subjectCtxKey,
		&clientClaims{Role: "customer"})
	// no subject set -> parse fails -> not ok
	if _, ok := clientSubject(ctx); ok {
		t.Error("claims with empty subject should not yield a subject")
	}

	good := &clientClaims{}
	good.Subject = uid.String()
	ctx = context.WithValue(context.Background(), subjectCtxKey, good)
	if sub, ok := clientSubject(ctx); !ok || sub != uid {
		t.Errorf("clientSubject = %v,%v want %v,true", sub, ok, uid)
	}

	// portal surface (no JWT claims in ctx) -> not ok, so ownership scoping skips.
	if _, ok := clientSubject(context.Background()); ok {
		t.Error("empty ctx must yield ok=false")
	}
}

// A forced-rotation token (pwc) reaches the password change and nothing else.
// Without this gate the flag only bars the portal, which customers never use.
func TestRequireJWTHoldsFlaggedTokenToPasswordChange(t *testing.T) {
	s := jwtServer("topsecret", time.Hour)
	flagged, _, _ := s.issueJWT(db.Principal{
		UserID: uuid.New(), Role: "customer", Username: "a", MustChangePassword: true,
	}, []string{"pwd"}, "")
	clean, _, _ := s.issueJWT(db.Principal{UserID: uuid.New(), Role: "customer", Username: "a"},
		[]string{"pwd"}, "")

	reached := false
	h := s.requireJWT(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	call := func(token, method, path string) (int, bool) {
		reached = false
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, reached
	}

	for _, tc := range []struct {
		method, path string
		wantCode     int
		wantReached  bool
	}{
		{http.MethodPost, "/me/password", 200, true},
		{http.MethodPost, "/auth/logout-all", 200, true},
		{http.MethodGet, "/me", http.StatusForbidden, false},
		{http.MethodPost, "/transfers", http.StatusForbidden, false},
		{http.MethodGet, "/me/password", http.StatusForbidden, false}, // allowlist is method-scoped
	} {
		if code, ok := call(flagged, tc.method, tc.path); code != tc.wantCode || ok != tc.wantReached {
			t.Errorf("flagged %s %s = %d (reached=%v), want %d (reached=%v)",
				tc.method, tc.path, code, ok, tc.wantCode, tc.wantReached)
		}
	}
	// An unflagged token is untouched.
	if code, ok := call(clean, http.MethodGet, "/me"); code != 200 || !ok {
		t.Errorf("unflagged GET /me = %d (reached=%v), want 200 (reached=true)", code, ok)
	}
}
