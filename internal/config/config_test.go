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
		// A typo'd mode must not fail OPEN by making servesAPI false and skipping
		// the JWT check — it is rejected outright, secret or not.
		{"unknown mode is rejected", "production", "portl", "s3cret", true},
		{"unknown mode is rejected in dev too", "development", "apii", "", true},
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

// LOG-LEVEL-DEFAULT: config.yaml is baked into the image, so whatever it says is
// what an unconfigured deployment logs at. It must not ship `debug` — the local
// compose stack opts back in with APP_LOGGING_LEVEL instead.
func TestShippedConfigLogsAtProductionLevel(t *testing.T) {
	cfg, err := LoadConfig("../..")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Logging.Level == "debug" {
		t.Errorf("config.yaml ships logging.level = debug; production pods would log at debug")
	}

	// The escape hatch the compose stack and the chart both rely on.
	t.Setenv("APP_LOGGING_LEVEL", "debug")
	cfg, err = LoadConfig("../..")
	if err != nil {
		t.Fatalf("LoadConfig with env override: %v", err)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("APP_LOGGING_LEVEL override = %q, want debug", cfg.Logging.Level)
	}
}
