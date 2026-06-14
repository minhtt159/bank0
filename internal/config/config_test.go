package config

import "testing"

// JWT-SECRET-FALLBACK: a non-development deployment MUST set an explicit JWT
// secret. Validate fails closed so a misconfigured prod can't silently run on the
// public dev constant (internal/api/jwt.go).
func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name      string
		env       string
		secret    string
		wantError bool
	}{
		{"dev without secret is allowed", "development", "", false},
		{"dev with secret is allowed", "development", "x", false},
		{"production without secret fails closed", "production", "", true},
		{"production with secret is allowed", "production", "s3cret", false},
		{"staging without secret fails closed", "staging", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cfg Config
			cfg.App.Env = c.env
			cfg.Auth.JWTSecret = c.secret
			err := cfg.Validate()
			if (err != nil) != c.wantError {
				t.Errorf("Validate() err = %v, wantError = %v", err, c.wantError)
			}
		})
	}
}
