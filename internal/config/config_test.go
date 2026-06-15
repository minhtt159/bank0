package config

import "testing"

// JWT-SECRET-FALLBACK: a non-development deployment MUST set an explicit JWT
// secret. Validate fails closed so a misconfigured prod can't silently run on the
// public dev constant (internal/api/jwt.go).
func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name      string
		env       string
		mode      string
		secret    string
		wantError bool
	}{
		{"dev without secret is allowed", "development", "all", "", false},
		{"dev with secret is allowed", "development", "all", "x", false},
		{"production api without secret fails closed", "production", "api", "", true},
		{"production all without secret fails closed", "production", "all", "", true},
		{"production with secret is allowed", "production", "api", "s3cret", false},
		{"staging api without secret fails closed", "staging", "api", "", true},
		// portal-only serves cookie sessions, not JWTs — no secret needed even in prod.
		{"production portal without secret is allowed", "production", "portal", "", false},
		// empty mode defaults to "all", which serves the api surface.
		{"production empty-mode without secret fails closed", "production", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cfg Config
			cfg.App.Env = c.env
			cfg.Server.Mode = c.mode
			cfg.Auth.JWTSecret = c.secret
			err := cfg.Validate()
			if (err != nil) != c.wantError {
				t.Errorf("Validate() err = %v, wantError = %v", err, c.wantError)
			}
		})
	}
}
